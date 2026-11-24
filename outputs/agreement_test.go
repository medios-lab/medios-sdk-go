package outputs

import (
	"encoding/json"
	"testing"

	"github.com/medios-lab/medios-sdk-go/medios"
)

// One record, one rule. The manifest's finish outcome and the medios
// package's FinishStatus are the same wire shape (DR-042) kept in two
// packages so that outputs has no medios dependency; this test feeds each
// payload to both validators and requires the same verdict, and the same
// decoded exit code, so the two cannot drift apart. The import is in a
// test file only.
func TestFinishOutcome_AgreesWithMediosFinishStatus(t *testing.T) {
	payloads := []string{
		`{"state":"RAN","exit_code":0}`,
		`{"state":"RAN","exit_code":3}`,
		`{"state":"RAN","signal":15}`,
		`{"state":"RAN"}`,
		`{"state":"RAN","exit_code":0,"signal":9}`,
		`{"state":"TIMED_OUT"}`,
		`{"state":"KILLED"}`,
		`{"state":"DID_NOT_START","reason":"the work spent the task's ceiling"}`,
		`{"state":"DID_NOT_START"}`,
		`{"state":"DID_NOT_START","reason":"ceiling","exit_code":0}`,
		`{"state":"LOST","reason":"wait status unreadable: EINTR"}`,
		`{"state":"LOST"}`,
		`{"state":"LOST","reason":"unreadable","signal":9}`,
		`{"state":"ran","exit_code":0}`,
		`{"state":"SKIPPED"}`,
		`{}`,
	}
	for _, raw := range payloads {
		var mine FinishOutcome
		if err := json.Unmarshal([]byte(raw), &mine); err != nil {
			t.Fatalf("%s: outputs decode: %v", raw, err)
		}
		m := manifest()
		m.Finish = mine
		mineErr := m.validate()

		var theirs medios.FinishStatus
		theirsErr := json.Unmarshal([]byte(raw), &theirs)
		if theirsErr == nil {
			theirsErr = medios.ValidateFinishOutcome(&theirs)
		}
		if (mineErr == nil) != (theirsErr == nil) {
			t.Errorf("%s: outputs says %v, medios says %v", raw, mineErr, theirsErr)
			continue
		}
		if mineErr != nil {
			continue
		}
		code, ran, err := theirs.Ran()
		if err != nil {
			t.Errorf("%s: medios Ran() errored on a record both accepted: %v", raw, err)
		}
		if ran != (mine.ExitCode != nil) || (ran && code != *mine.ExitCode) {
			t.Errorf("%s: exit code disagrees: outputs %v, medios %d,%v", raw, mine.ExitCode, code, ran)
		}
	}
}
