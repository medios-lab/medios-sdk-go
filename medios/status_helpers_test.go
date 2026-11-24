package medios

import "testing"

func TestTaskStatus_WorkExitCode(t *testing.T) {
	cases := []struct {
		name     string
		status   TaskStatus
		wantCode int
		wantOK   bool
	}{
		{
			name:     "finished reports zero exit code",
			status:   TaskStatus{State: TaskFinished, Message: ""},
			wantCode: 0,
			wantOK:   true,
		},
		{
			name:     "failed command exited with status 1",
			status:   TaskStatus{State: TaskFailed, Message: "Command exited with status 1"},
			wantCode: 1,
			wantOK:   true,
		},
		{
			name:     "failed command exited with status 137",
			status:   TaskStatus{State: TaskFailed, Message: "Command exited with status 137 (OOM)"},
			wantCode: 137,
			wantOK:   true,
		},
		{
			name:     "failed exited with code 42 case-insensitive",
			status:   TaskStatus{State: TaskFailed, Message: "task exited with code 42"},
			wantCode: 42,
			wantOK:   true,
		},
		{
			name:     "failed exit status 2",
			status:   TaskStatus{State: TaskFailed, Message: "process failed: exit status 2"},
			wantCode: 2,
			wantOK:   true,
		},
		{
			name:     "failed exit code negative",
			status:   TaskStatus{State: TaskFailed, Message: "Command exited with status -1"},
			wantCode: -1,
			wantOK:   true,
		},
		{
			name:     "failed by signal has no exit code",
			status:   TaskStatus{State: TaskFailed, Message: "Command terminated with signal Aborted"},
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "failed with empty message has no exit code",
			status:   TaskStatus{State: TaskFailed, Message: ""},
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "killed never reports work exit code even if message mentions status",
			status:   TaskStatus{State: TaskKilled, Message: "Command exited with status 143"},
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "non-terminal and infrastructure states return false",
			status:   TaskStatus{State: TaskRunning, Message: "Command exited with status 0"},
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "task lost returns false",
			status:   TaskStatus{State: TaskLost, Message: "Node lost"},
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "task error returns false",
			status:   TaskStatus{State: TaskError, Reason: "REASON_TASK_INVALID", Message: "invalid task"},
			wantCode: 0,
			wantOK:   false,
		},
		{
			name:     "task starting and unreachable return false",
			status:   TaskStatus{State: TaskUnreachable},
			wantCode: 0,
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, ok := tc.status.WorkExitCode()
			if ok != tc.wantOK || code != tc.wantCode {
				t.Fatalf("WorkExitCode() = (%d, %v), want (%d, %v)", code, ok, tc.wantCode, tc.wantOK)
			}
		})
	}
}

func TestTaskStatus_StoreUnitRefused(t *testing.T) {
	cases := []struct {
		name      string
		status    TaskStatus
		wantStore string
		wantOK    bool
	}{
		{
			name: "no free readers unit with REASON_TASK_INVALID",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Task uses store 'datasets-v2' which has no free readers unit on node n-1 (readers 4/4 in use)",
			},
			wantStore: "datasets-v2",
			wantOK:    true,
		},
		{
			name: "no free writers unit with empty reason",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "",
				Message: "Task uses store 'checkpoints_prod' which has no free writers unit on node n-2 (writers 1/1 in use)",
			},
			wantStore: "checkpoints_prod",
			wantOK:    true,
		},
		{
			name: "exclusive request while units held",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Task uses store 'scratch.local' which is requested exclusive and has 1 active writer unit on node n-1",
			},
			wantStore: "scratch.local",
			wantOK:    true,
		},
		{
			name: "medios#535 Store phrasing no free readers unit",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Store 'datasets-v2' has no free readers unit on node n-1: 4 of 4 held",
			},
			wantStore: "datasets-v2",
			wantOK:    true,
		},
		{
			name: "medios#535 Store phrasing no free writers unit",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Store 'checkpoints_prod' has no free writers unit on node n-2: 2 of 2 held",
			},
			wantStore: "checkpoints_prod",
			wantOK:    true,
		},
		{
			name: "medios#535 Store phrasing exclusive request",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Store 'scratch.local' is requested exclusive and has 1 of 10 writers units held on node n-1",
			},
			wantStore: "scratch.local",
			wantOK:    true,
		},
		{
			name: "static read-only declaration error does not match",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Store 'tabicl-tables' is read-only by declaration on node n-1 (writers 0); request it RO",
			},
			wantStore: "",
			wantOK:    false,
		},
		{
			name: "static declares 0 writer units error does not match",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "store tabicl-tables declares 0 writer units on node n-1, task asks 1",
			},
			wantStore: "",
			wantOK:    false,
		},
		{
			name: "store not on node does not match",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_TASK_INVALID",
				Message: "Task uses store 'missing' which is not on node n-1",
			},
			wantStore: "",
			wantOK:    false,
		},
		{
			name: "non-TaskError state does not match",
			status: TaskStatus{
				State:   TaskFailed,
				Reason:  "REASON_TASK_INVALID",
				Message: "Task uses store 'datasets-v2' which has no free readers unit",
			},
			wantStore: "",
			wantOK:    false,
		},
		{
			name: "other TaskError reason does not match",
			status: TaskStatus{
				State:   TaskError,
				Reason:  "REASON_RECONCILIATION",
				Message: "Task uses store 'datasets-v2' which has no free readers unit",
			},
			wantStore: "",
			wantOK:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, ok := tc.status.StoreUnitRefused()
			if ok != tc.wantOK || store != tc.wantStore {
				t.Fatalf("StoreUnitRefused() = (%q, %v), want (%q, %v)", store, ok, tc.wantStore, tc.wantOK)
			}
		})
	}
}
