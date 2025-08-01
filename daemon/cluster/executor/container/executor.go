package container

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/filters"
	"github.com/moby/moby/api/types/network"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/daemon/cluster/controllers/plugin"
	"github.com/moby/moby/v2/daemon/cluster/convert"
	executorpkg "github.com/moby/moby/v2/daemon/cluster/executor"
	clustertypes "github.com/moby/moby/v2/daemon/cluster/provider"
	"github.com/moby/moby/v2/daemon/libnetwork"
	networktypes "github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/swarmkit/v2/agent"
	"github.com/moby/swarmkit/v2/agent/exec"
	"github.com/moby/swarmkit/v2/api"
	"github.com/moby/swarmkit/v2/api/naming"
	swarmlog "github.com/moby/swarmkit/v2/log"
	"github.com/moby/swarmkit/v2/template"
	"github.com/pkg/errors"
)

type executor struct {
	backend       executorpkg.Backend
	imageBackend  executorpkg.ImageBackend
	pluginBackend plugin.Backend
	volumeBackend executorpkg.VolumeBackend
	dependencies  exec.DependencyManager
	mutex         sync.Mutex // This mutex protects the following node field
	node          *api.NodeDescription

	// nodeObj holds a copy of the swarmkit Node object from the time of the
	// last call to executor.Configure. This allows us to discover which
	// network attachments the node previously had, which further allows us to
	// determine which, if any, need to be removed. nodeObj is not protected by
	// a mutex, because it is only written to in the method (Configure) that it
	// is read from. If that changes, it may need to be guarded.
	nodeObj *api.Node
}

// NewExecutor returns an executor from the docker client.
func NewExecutor(b executorpkg.Backend, p plugin.Backend, i executorpkg.ImageBackend, v executorpkg.VolumeBackend) exec.Executor {
	return &executor{
		backend:       b,
		pluginBackend: p,
		imageBackend:  i,
		volumeBackend: v,
		dependencies:  agent.NewDependencyManager(convert.SwarmPluginGetter(b.PluginGetter())),
	}
}

// Describe returns the underlying node description from the docker client.
func (e *executor) Describe(ctx context.Context) (*api.NodeDescription, error) {
	info, err := e.backend.SystemInfo(ctx)
	if err != nil {
		return nil, err
	}

	plugins := map[api.PluginDescription]struct{}{}
	addPlugins := func(typ string, names []string) {
		for _, name := range names {
			plugins[api.PluginDescription{
				Type: typ,
				Name: name,
			}] = struct{}{}
		}
	}

	// add v1 plugins
	addPlugins("Volume", info.Plugins.Volume)
	// Add builtin driver "overlay" (the only builtin multi-host driver) to
	// the plugin list by default.
	addPlugins("Network", append([]string{"overlay"}, info.Plugins.Network...))
	addPlugins("Authorization", info.Plugins.Authorization)
	addPlugins("Log", info.Plugins.Log)

	// add v2 plugins
	v2Plugins, err := e.backend.PluginManager().List(filters.NewArgs())
	if err == nil {
		for _, plgn := range v2Plugins {
			for _, typ := range plgn.Config.Interface.Types {
				if typ.Prefix != "docker" || !plgn.Enabled {
					continue
				}
				plgnTyp := typ.Capability
				switch typ.Capability {
				case "volumedriver":
					plgnTyp = "Volume"
				case "networkdriver":
					plgnTyp = "Network"
				case "logdriver":
					plgnTyp = "Log"
				}

				plugins[api.PluginDescription{
					Type: plgnTyp,
					Name: plgn.Name,
				}] = struct{}{}
			}
		}
	}

	pluginFields := make([]api.PluginDescription, 0, len(plugins))
	for k := range plugins {
		pluginFields = append(pluginFields, k)
	}

	sort.Sort(sortedPlugins(pluginFields))

	// parse []string labels into a map[string]string
	labels := map[string]string{}
	for _, l := range info.Labels {
		k, v, ok := strings.Cut(l, "=")
		// this will take the last value in the list for a given key
		// ideally, one shouldn't assign multiple values to the same key
		if ok {
			labels[k] = v
		}
	}

	// TODO(dperny): don't ignore the error here
	csiInfo, _ := e.Volumes().Plugins().NodeInfo(ctx)

	description := &api.NodeDescription{
		Hostname: info.Name,
		Platform: &api.Platform{
			Architecture: info.Architecture,
			OS:           info.OSType,
		},
		Engine: &api.EngineDescription{
			EngineVersion: info.ServerVersion,
			Labels:        labels,
			Plugins:       pluginFields,
		},
		Resources: &api.Resources{
			NanoCPUs:    int64(info.NCPU) * 1e9,
			MemoryBytes: info.MemTotal,
			Generic:     convert.GenericResourcesToGRPC(info.GenericResources),
		},
		CSIInfo: csiInfo,
	}

	// Save the node information in the executor field
	e.mutex.Lock()
	e.node = description
	e.mutex.Unlock()

	return description, nil
}

func (e *executor) Configure(ctx context.Context, node *api.Node) error {
	var ingressNA *api.NetworkAttachment
	attachments := make(map[string]string)

	for _, na := range node.Attachments {
		if na == nil || na.Network == nil || len(na.Addresses) == 0 {
			// this should not happen, but we got a panic here and don't have a
			// good idea about what the underlying data structure looks like.
			swarmlog.G(ctx).WithField("NetworkAttachment", fmt.Sprintf("%#v", na)).Warn("skipping nil or malformed node network attachment entry")
			continue
		}

		if na.Network.Spec.Ingress {
			ingressNA = na
		}

		attachments[na.Network.ID] = na.Addresses[0]
	}

	// discover which, if any, attachments have been removed.
	//
	// we aren't responsible directly for creating these networks. that is
	// handled indirectly when a container using that network is created.
	// however, when it comes time to remove the network, none of the relevant
	// tasks may exist anymore. this means we should go ahead and try to remove
	// any network we know to no longer be in use.

	// removeAttachments maps the network ID to a boolean. This boolean
	// indicates whether the attachment in question is totally removed (true),
	// or has just had its IP changed (false)
	removeAttachments := make(map[string]bool)

	// the first time we Configure, nodeObj wil be nil, because it will not be
	// set yet. in that case, skip this check.
	if e.nodeObj != nil {
		for _, na := range e.nodeObj.Attachments {
			// same thing as above, check sanity of the attachments so we don't
			// get a panic.
			if na == nil || na.Network == nil || len(na.Addresses) == 0 {
				swarmlog.G(ctx).WithField("NetworkAttachment", fmt.Sprintf("%#v", na)).Warn("skipping nil or malformed node network attachment entry")
				continue
			}

			// now, check if the attachment exists and shares the same IP address.
			if ip, ok := attachments[na.Network.ID]; !ok || na.Addresses[0] != ip {
				// if the map entry exists, then the network still exists, and the
				// IP must be what has changed
				removeAttachments[na.Network.ID] = !ok
			}
		}
	}

	if (ingressNA == nil) && (node.Attachment != nil) && (len(node.Attachment.Addresses) > 0) {
		ingressNA = node.Attachment
		attachments[ingressNA.Network.ID] = ingressNA.Addresses[0]
	}

	if ingressNA == nil {
		e.backend.ReleaseIngress()
	} else {
		options := network.CreateOptions{
			Driver: ingressNA.Network.DriverState.Name,
			IPAM: &network.IPAM{
				Driver: ingressNA.Network.IPAM.Driver.Name,
			},
			Options: ingressNA.Network.DriverState.Options,
			Ingress: true,
		}

		for _, ic := range ingressNA.Network.IPAM.Configs {
			c := network.IPAMConfig{
				Subnet:  ic.Subnet,
				IPRange: ic.Range,
				Gateway: ic.Gateway,
			}
			options.IPAM.Config = append(options.IPAM.Config, c)
		}

		_, err := e.backend.SetupIngress(clustertypes.NetworkCreateRequest{
			ID: ingressNA.Network.ID,
			CreateRequest: network.CreateRequest{
				Name:          ingressNA.Network.Spec.Annotations.Name,
				CreateOptions: options,
			},
		}, ingressNA.Addresses[0])
		if err != nil {
			return err
		}
	}

	var (
		activeEndpointsError *libnetwork.ActiveEndpointsError
		errNoSuchNetwork     libnetwork.ErrNoSuchNetwork
	)

	// now, finally, remove any network LB attachments that we no longer have.
	for nw, gone := range removeAttachments {
		err := e.backend.DeleteManagedNetwork(nw)
		switch {
		case err == nil:
			continue
		case errors.As(err, &activeEndpointsError):
			// this is the purpose of the boolean in the map. it's literally
			// just to log an appropriate, informative error. i'm unsure if
			// this can ever actually occur, but we need to know if it does.
			if gone {
				swarmlog.G(ctx).Warnf("network %s should be removed, but still has active attachments", nw)
			} else {
				swarmlog.G(ctx).Warnf("network %s should have its node LB IP changed, but cannot be removed because of active attachments", nw)
			}
			continue
		case errors.As(err, &errNoSuchNetwork):
			// NoSuchNetworkError indicates the network is already gone.
			continue
		default:
			swarmlog.G(ctx).Errorf("network %s remove failed: %v", nw, err)
		}
	}

	// now update our copy of the node object, reset the attachment store, and
	// return
	e.nodeObj = node

	return e.backend.GetAttachmentStore().ResetAttachments(attachments)
}

// Controller returns a docker container runner.
func (e *executor) Controller(t *api.Task) (exec.Controller, error) {
	dependencyGetter := template.NewTemplatedDependencyGetter(agent.Restrict(e.dependencies, t), t, nil)

	// Get the node description from the executor field
	e.mutex.Lock()
	nodeDescription := e.node
	e.mutex.Unlock()

	if t.Spec.GetAttachment() != nil {
		return newNetworkAttacherController(e.backend, e.imageBackend, e.volumeBackend, t, nodeDescription, dependencyGetter)
	}

	var ctlr exec.Controller
	switch r := t.Spec.GetRuntime().(type) {
	case *api.TaskSpec_Generic:
		swarmlog.G(context.TODO()).WithFields(log.Fields{
			"kind":     r.Generic.Kind,
			"type_url": r.Generic.Payload.TypeUrl,
		}).Debug("custom runtime requested")
		runtimeKind, err := naming.Runtime(t.Spec)
		if err != nil {
			return ctlr, err
		}
		switch runtimeKind {
		case string(swarmtypes.RuntimePlugin):
			if !e.backend.HasExperimental() {
				return ctlr, fmt.Errorf("runtime type %q only supported in experimental", swarmtypes.RuntimePlugin)
			}
			c, err := plugin.NewController(e.pluginBackend, t)
			if err != nil {
				return ctlr, err
			}
			ctlr = c
		default:
			return ctlr, fmt.Errorf("unsupported runtime type: %q", runtimeKind)
		}
	case *api.TaskSpec_Container:
		c, err := newController(e.backend, e.imageBackend, e.volumeBackend, t, nodeDescription, dependencyGetter)
		if err != nil {
			return ctlr, err
		}
		ctlr = c
	default:
		return ctlr, fmt.Errorf("unsupported runtime: %q", r)
	}

	return ctlr, nil
}

func (e *executor) SetNetworkBootstrapKeys(keys []*api.EncryptionKey) error {
	nwKeys := []*networktypes.EncryptionKey{}
	for _, key := range keys {
		nwKey := &networktypes.EncryptionKey{
			Subsystem:   key.Subsystem,
			Algorithm:   int32(key.Algorithm),
			Key:         make([]byte, len(key.Key)),
			LamportTime: key.LamportTime,
		}
		copy(nwKey.Key, key.Key)
		nwKeys = append(nwKeys, nwKey)
	}
	e.backend.SetNetworkBootstrapKeys(nwKeys)

	return nil
}

func (e *executor) Secrets() exec.SecretsManager {
	return e.dependencies.Secrets()
}

func (e *executor) Configs() exec.ConfigsManager {
	return e.dependencies.Configs()
}

func (e *executor) Volumes() exec.VolumesManager {
	return e.dependencies.Volumes()
}

type sortedPlugins []api.PluginDescription

func (sp sortedPlugins) Len() int { return len(sp) }

func (sp sortedPlugins) Swap(i, j int) { sp[i], sp[j] = sp[j], sp[i] }

func (sp sortedPlugins) Less(i, j int) bool {
	if sp[i].Type != sp[j].Type {
		return sp[i].Type < sp[j].Type
	}
	return sp[i].Name < sp[j].Name
}
