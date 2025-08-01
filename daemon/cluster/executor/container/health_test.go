//go:build !windows

package container

import (
	"context"
	"errors"
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	eventtypes "github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/events"
	"github.com/moby/swarmkit/v2/api"
)

func TestHealthStates(t *testing.T) {
	// set up environment: events, task, container ....
	e := events.New()
	_, l, _ := e.Subscribe()
	defer e.Evict(l)

	task := &api.Task{
		ID:        "id",
		ServiceID: "sid",
		Spec: api.TaskSpec{
			Runtime: &api.TaskSpec_Container{
				Container: &api.ContainerSpec{
					Image: "image_name",
					Labels: map[string]string{
						"com.docker.swarm.task.id": "id",
					},
				},
			},
		},
		Annotations: api.Annotations{Name: "name"},
	}

	c := &container.Container{
		ID:   "id",
		Name: "name",
		Config: &containertypes.Config{
			Image: "image_name",
			Labels: map[string]string{
				"com.docker.swarm.task.id": "id",
			},
		},
	}

	daemon := &daemon.Daemon{
		EventsService: e,
	}

	controller, err := newController(daemon, nil, nil, task, nil, nil)
	if err != nil {
		t.Fatalf("create controller fail %v", err)
	}

	errChan := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// fire checkHealth
	go func() {
		err := controller.checkHealth(ctx)
		select {
		case errChan <- err:
		case <-ctx.Done():
		}
	}()

	// send an event and expect to get expectedErr
	// if expectedErr is nil, shouldn't get any error
	logAndExpect := func(msg eventtypes.Action, expectedErr error) {
		daemon.LogContainerEvent(c, msg)

		timer := time.NewTimer(1 * time.Second)
		defer timer.Stop()

		select {
		case err := <-errChan:
			if !errors.Is(err, expectedErr) {
				t.Fatalf("expect error %v, but get %v", expectedErr, err)
			}
		case <-timer.C:
			if expectedErr != nil {
				t.Fatal("time limit exceeded, didn't get expected error")
			}
		}
	}

	// events that are ignored by checkHealth
	logAndExpect(eventtypes.ActionHealthStatusRunning, nil)
	logAndExpect(eventtypes.ActionHealthStatusHealthy, nil)
	logAndExpect(eventtypes.ActionDie, nil)

	// unhealthy event will be caught by checkHealth
	logAndExpect(eventtypes.ActionHealthStatusUnhealthy, ErrContainerUnhealthy)
}
