package container

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/versions"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/testutil"
	"github.com/moby/moby/v2/testutil/daemon"
	"github.com/moby/moby/v2/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/skip"
)

// testIpcCheckDevExists checks whether a given mount (identified by its
// major:minor pair from /proc/self/mountinfo) exists on the host system.
//
// The format of /proc/self/mountinfo is like:
//
//	29 23 0:24 / /dev/shm rw,nosuid,nodev shared:4 - tmpfs tmpfs rw
//	^^^^\
//	     - this is the minor:major we look for
//
//nolint:dupword
func testIpcCheckDevExists(mm string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 7 {
			continue
		}
		if fields[2] == mm {
			return true, nil
		}
	}

	return false, s.Err()
}

// testIpcNonePrivateShareable is a helper function to test "none",
// "private" and "shareable" modes.
func testIpcNonePrivateShareable(t *testing.T, mode string, mustBeMounted bool, mustBeShared bool) {
	ctx := setupTest(t)

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	hostCfg := containertypes.HostConfig{
		IpcMode: containertypes.IpcMode(mode),
	}
	apiClient := testEnv.APIClient()

	resp, err := apiClient.ContainerCreate(ctx, &cfg, &hostCfg, nil, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))

	err = apiClient.ContainerStart(ctx, resp.ID, containertypes.StartOptions{})
	assert.NilError(t, err)

	// get major:minor pair for /dev/shm from container's /proc/self/mountinfo
	cmd := "awk '($5 == \"/dev/shm\") {printf $3}' /proc/self/mountinfo"
	result, err := container.Exec(ctx, apiClient, resp.ID, []string{"sh", "-c", cmd})
	assert.NilError(t, err)
	mm := result.Combined()
	if !mustBeMounted {
		assert.Check(t, is.Equal(mm, ""))
		// no more checks to perform
		return
	}
	assert.Check(t, is.Equal(true, regexp.MustCompile("^[0-9]+:[0-9]+$").MatchString(mm)))

	shared, err := testIpcCheckDevExists(mm)
	assert.NilError(t, err)
	t.Logf("[testIpcPrivateShareable] ipcmode: %v, ipcdev: %v, shared: %v, mustBeShared: %v\n", mode, mm, shared, mustBeShared)
	assert.Check(t, is.Equal(shared, mustBeShared))
}

// TestIpcModeNone checks the container "none" IPC mode
// (--ipc none) works as expected. It makes sure there is no
// /dev/shm mount inside the container.
func TestIpcModeNone(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcNonePrivateShareable(t, "none", false, false)
}

// TestIpcModePrivate checks the container private IPC mode
// (--ipc private) works as expected. It gets the minor:major pair
// of /dev/shm mount from the container, and makes sure there is no
// such pair on the host.
func TestIpcModePrivate(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcNonePrivateShareable(t, "private", true, false)
}

// TestIpcModeShareable checks the container shareable IPC mode
// (--ipc shareable) works as expected. It gets the minor:major pair
// of /dev/shm mount from the container, and makes sure such pair
// also exists on the host.
func TestIpcModeShareable(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "no support for --ipc=shareable in rootless")

	testIpcNonePrivateShareable(t, "shareable", true, true)
}

// testIpcContainer is a helper function to test --ipc container:NNN mode in various scenarios
func testIpcContainer(t *testing.T, donorMode string, mustWork bool) {
	t.Helper()

	ctx := setupTest(t)

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	hostCfg := containertypes.HostConfig{
		IpcMode: containertypes.IpcMode(donorMode),
	}
	apiClient := testEnv.APIClient()

	// create and start the "donor" container
	resp, err := apiClient.ContainerCreate(ctx, &cfg, &hostCfg, nil, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))
	name1 := resp.ID

	err = apiClient.ContainerStart(ctx, name1, containertypes.StartOptions{})
	assert.NilError(t, err)

	// create and start the second container
	hostCfg.IpcMode = containertypes.IpcMode("container:" + name1)
	resp, err = apiClient.ContainerCreate(ctx, &cfg, &hostCfg, nil, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))
	name2 := resp.ID

	err = apiClient.ContainerStart(ctx, name2, containertypes.StartOptions{})
	if !mustWork {
		// start should fail with a specific error
		assert.Check(t, is.ErrorContains(err, "non-shareable IPC"))
		// no more checks to perform here
		return
	}

	// start should succeed
	assert.NilError(t, err)

	// check that IPC is shared
	// 1. create a file in the first container
	_, err = container.Exec(ctx, apiClient, name1, []string{"sh", "-c", "printf covfefe > /dev/shm/bar"})
	assert.NilError(t, err)
	// 2. check it's the same file in the second one
	result, err := container.Exec(ctx, apiClient, name2, []string{"cat", "/dev/shm/bar"})
	assert.NilError(t, err)
	out := result.Combined()
	assert.Check(t, is.Equal(true, regexp.MustCompile("^covfefe$").MatchString(out)))
}

// TestAPIIpcModeShareableAndContainer checks that
// 1) a container created with --ipc container:ID can use IPC of another shareable container.
// 2) a container created with --ipc container:ID can NOT use IPC of another private container.
func TestAPIIpcModeShareableAndContainer(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testIpcContainer(t, "shareable", true)

	testIpcContainer(t, "private", false)
}

/* TestAPIIpcModeHost checks that a container created with --ipc host
 * can use IPC of the host system.
 */
func TestAPIIpcModeHost(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsUserNamespace)

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}
	hostCfg := containertypes.HostConfig{
		IpcMode: containertypes.IPCModeHost,
	}

	ctx := testutil.StartSpan(baseContext, t)

	apiClient := testEnv.APIClient()
	resp, err := apiClient.ContainerCreate(ctx, &cfg, &hostCfg, nil, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))
	name := resp.ID

	err = apiClient.ContainerStart(ctx, name, containertypes.StartOptions{})
	assert.NilError(t, err)

	// check that IPC is shared
	// 1. create a file inside container
	_, err = container.Exec(ctx, apiClient, name, []string{"sh", "-c", "printf covfefe > /dev/shm/." + name})
	assert.NilError(t, err)
	// 2. check it's the same on the host
	bytes, err := os.ReadFile("/dev/shm/." + name)
	assert.NilError(t, err)
	assert.Check(t, is.Equal("covfefe", string(bytes)))
	// 3. clean up
	_, err = container.Exec(ctx, apiClient, name, []string{"rm", "-f", "/dev/shm/." + name})
	assert.NilError(t, err)
}

// testDaemonIpcPrivateShareable is a helper function to test "private" and "shareable" daemon default ipc modes.
func testDaemonIpcPrivateShareable(t *testing.T, mustBeShared bool, arg ...string) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, arg...)
	defer d.Stop(t)

	c := d.NewClientT(t)

	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"top"},
	}

	resp, err := c.ContainerCreate(ctx, &cfg, &containertypes.HostConfig{}, nil, nil, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(resp.Warnings), 0))

	err = c.ContainerStart(ctx, resp.ID, containertypes.StartOptions{})
	assert.NilError(t, err)

	// get major:minor pair for /dev/shm from container's /proc/self/mountinfo
	cmd := "awk '($5 == \"/dev/shm\") {printf $3}' /proc/self/mountinfo"
	result, err := container.Exec(ctx, c, resp.ID, []string{"sh", "-c", cmd})
	assert.NilError(t, err)
	mm := result.Combined()
	assert.Check(t, is.Equal(true, regexp.MustCompile("^[0-9]+:[0-9]+$").MatchString(mm)))

	shared, err := testIpcCheckDevExists(mm)
	assert.NilError(t, err)
	t.Logf("[testDaemonIpcPrivateShareable] ipcdev: %v, shared: %v, mustBeShared: %v\n", mm, shared, mustBeShared)
	assert.Check(t, is.Equal(shared, mustBeShared))
}

// TestDaemonIpcModeShareable checks that --default-ipc-mode shareable works as intended.
func TestDaemonIpcModeShareable(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "no support for --ipc=shareable in rootless")

	testDaemonIpcPrivateShareable(t, true, "--default-ipc-mode", "shareable")
}

// TestDaemonIpcModePrivate checks that --default-ipc-mode private works as intended.
func TestDaemonIpcModePrivate(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testDaemonIpcPrivateShareable(t, false, "--default-ipc-mode", "private")
}

// used to check if an IpcMode given in config works as intended
func testDaemonIpcFromConfig(t *testing.T, mode string, mustExist bool) {
	config := `{"default-ipc-mode": "` + mode + `"}`
	// WithMode is needed for rootless
	file := fs.NewFile(t, "test-daemon-ipc-config", fs.WithContent(config), fs.WithMode(0o644))

	testDaemonIpcPrivateShareable(t, mustExist, "--config-file", file.Path())
}

// TestDaemonIpcModePrivateFromConfig checks that "default-ipc-mode: private" config works as intended.
func TestDaemonIpcModePrivateFromConfig(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	testDaemonIpcFromConfig(t, "private", false)
}

// TestDaemonIpcModeShareableFromConfig checks that "default-ipc-mode: shareable" config works as intended.
func TestDaemonIpcModeShareableFromConfig(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "no support for --ipc=shareable in rootless")

	testDaemonIpcFromConfig(t, "shareable", true)
}

// TestIpcModeOlderClient checks that older client gets shareable IPC mode
// by default, even when the daemon default is private.
func TestIpcModeOlderClient(t *testing.T) {
	apiClient := testEnv.APIClient()
	skip.If(t, versions.LessThan(apiClient.ClientVersion(), "1.40"), "requires client API >= 1.40")

	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	// pre-check: default ipc mode in daemon is private
	cID := container.Create(ctx, t, apiClient, container.WithAutoRemove)

	inspect, err := apiClient.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(string(inspect.HostConfig.IpcMode), "private"))

	// main check: using older client creates "shareable" container
	apiClient = request.NewAPIClient(t, client.WithVersion("1.39"))
	cID = container.Create(ctx, t, apiClient, container.WithAutoRemove)

	inspect, err = apiClient.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(string(inspect.HostConfig.IpcMode), "shareable"))
}
