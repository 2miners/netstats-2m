package camel

import (
	"encoding/json"
	"os"
	"testing"
)

// testdata.json holds [input, lodash 4.18.1 _.camelCase(input)] pairs.
func TestCaseMatchesLodash(t *testing.T) {
	b, err := os.ReadFile("testdata.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]string
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if got := Case(c[0]); got != c[1] {
			t.Errorf("Case(%q) = %q, lodash gives %q", c[0], got, c[1])
		}
	}
}
