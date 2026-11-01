package cli

import "testing"

func TestTreeRunRequestChecksResume(t *testing.T) {
	base := `{"task":"t","envelope":{},"tools":["help"]`
	for _, tc := range []struct{ body, want string }{
		{base + `}`, ""},
		{base + `,"resume":"abc-123.x"}`, ""},
		{base + `,"resume":""}`, "resume must be a session id."},
		{base + `,"resume":"../x"}`, "resume must be a session id."},
		{base + `,"resume":7}`, "resume must be a session id."},
	} {
		if _, why := treeRunRequest([]byte(tc.body)); why != tc.want {
			t.Errorf("%s: got %q want %q", tc.body, why, tc.want)
		}
	}
}
