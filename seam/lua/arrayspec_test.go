package luawire_test

import (
	"strconv"
	"strings"
	"testing"
)

// TestArrayTaskCount exercises job_submit.lua's array_task_count parser (exposed
// as the global obol_array_task_count) against the Slurm array-spec forms —
// ranges, %throttle, step ranges, comma lists — verified against a live cluster
// where the spec arrives as job_desc.array_inx (#103). It loads the real shim
// under lua with the slurm host and the obol_wire/obol_transport requires stubbed,
// so the tested code is exactly what runs in slurmctld.
func TestArrayTaskCount(t *testing.T) {
	lua := luaBin(t)

	cases := []struct {
		spec string
		want int
	}{
		{"", 1},            // non-array job
		{"0-3", 4},         // simple range
		{"0-9", 10},        //
		{"0-9%4", 10},      // %throttle is a concurrency cap, not a task count
		{"5", 1},           // single index
		{"1,3,5", 3},       // list
		{"0-3,7,10-12", 8}, // mixed list: 4 + 1 + 3
		{"0-9:2", 5},       // step range: 0,2,4,6,8
		{"0-8:3", 3},       // 0,3,6
		{"garbage", 1},     // unparseable -> fail-safe single
		{"2-1", 1},         // inverted range -> no tasks -> fail-safe single
	}
	for _, tc := range cases {
		// Stub the two requires and the slurm host, load the shim, print the count.
		script := `
package.preload['obol_wire'] = function() return {} end
package.preload['obol_transport'] = function() return {} end
slurm = { SUCCESS = 0, ERROR = -1, log_info = function() end }
dofile('job_submit.lua')
io.write(tostring(obol_array_task_count([==[` + tc.spec + `]==])))
`
		got := strings.TrimSpace(string(runLua(t, lua, script)))
		n, err := strconv.Atoi(got)
		if err != nil {
			t.Fatalf("spec %q: non-numeric result %q", tc.spec, got)
		}
		if n != tc.want {
			t.Errorf("array_task_count(%q) = %d, want %d", tc.spec, n, tc.want)
		}
	}
}

// TestParseSources exercises job_submit.lua's --comment source-list parser
// (obol_parse_sources) — the #98 convention "obol-sources=a,b,c". want is the
// comma-joined result, or "nil" when the comment names no sources.
func TestParseSources(t *testing.T) {
	lua := luaBin(t)
	cases := []struct {
		comment string
		want    string
	}{
		{"", "nil"},                                     // no comment
		{"just a note", "nil"},                          // unrelated comment
		{"obol-sources=grant", "grant"},                 // single
		{"obol-sources=grant,startup", "grant,startup"}, // ordered pair
		{"obol-sources=grant,startup,disc", "grant,startup,disc"},
		{"note; obol-sources=grant,startup", "grant,startup"},      // token amid other text
		{"obol-sources=grant,startup ; trailing", "grant,startup"}, // stops at delimiter
		{"obol-sources=", "nil"},                                   // empty value
		{"obol-sources= grant , startup ", "nil"},                  // spaces after = break the token (documented: no spaces)
	}
	for _, tc := range cases {
		script := `
package.preload['obol_wire'] = function() return {} end
package.preload['obol_transport'] = function() return {} end
slurm = { SUCCESS = 0, ERROR = -1, log_info = function() end }
dofile('job_submit.lua')
local r = obol_parse_sources([==[` + tc.comment + `]==])
if r == nil then io.write("nil") else io.write(table.concat(r, ",")) end
`
		got := strings.TrimSpace(string(runLua(t, lua, script)))
		if got != tc.want {
			t.Errorf("parse_sources(%q) = %q, want %q", tc.comment, got, tc.want)
		}
	}
}
