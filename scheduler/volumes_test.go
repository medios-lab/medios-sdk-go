package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/medios-lab/medios-sdk-go/medios"
)

// Accept refuses a task with a host-path volume before anything is sent: the
// client points at nothing reachable and the error is the volume check, not
// a dial failure.
func TestAccept_RefusesHostPathBeforeSending(t *testing.T) {
	c, err := NewClient("http://127.0.0.1:1", "fw", WithUser("u"))
	if err != nil {
		t.Fatal(err)
	}
	task, err := medios.NewTaskInfo("team:research", "t", medios.ID{Value: "t-1"}, medios.ID{Value: "S0"}, nil,
		medios.CommandInfo{Shell: true, Value: "true"})
	if err != nil {
		t.Fatal(err)
	}
	task.Container = &medios.ContainerInfo{Type: medios.ContainerDocker,
		Docker: &medios.DockerInfo{Image: "alpine"},
		Volumes: []medios.Volume{{ContainerPath: "/data", Mode: medios.VolumeRW,
			Source: &medios.VolumeSource{Type: medios.VolumeSourceHostPath}}}}
	err = c.Accept(context.Background(), []medios.ID{{Value: "O-1"}}, []medios.TaskInfo{task})
	if err == nil || !strings.Contains(err.Error(), "task t-1: volume /data has a HOST_PATH source") {
		t.Fatalf("err = %v", err)
	}
}
