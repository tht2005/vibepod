package ui

import (
	"reflect"
	"testing"
)

func kinds(evs []Event) []Kind {
	var k []Kind
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestParserFindsTheBoundaries(t *testing.T) {
	var p Parser
	in := "motd\x1b]133;D;0\a\x1b]7717;home=/h;cwd=/h/a;b\a\x1b]133;A\a$ \x1b]133;B\a" +
		"ls\r\n\x1b]133;C\a\x1b[1mfile\x1b[0m\r\n\x1b]133;D;2\x1b\\"
	evs := p.Feed([]byte(in))
	want := []Kind{Text, Done, Cwd, Prompt, Text, Input, Text, Exec, Text, Done}
	if !reflect.DeepEqual(kinds(evs), want) {
		t.Fatalf("kinds = %v, want %v", kinds(evs), want)
	}
	if evs[2].Home != "/h" || evs[2].Cwd != "/h/a;b" {
		t.Errorf("cwd = %+v", evs[2])
	}
	if string(evs[8].Data) != "\x1b[1mfile\x1b[0m\r\n" {
		t.Errorf("output = %q", evs[8].Data)
	}
	if evs[9].Code != 2 {
		t.Errorf("code = %d", evs[9].Code)
	}
}

// A read boundary can fall anywhere, including inside a marker.
func TestParserCarriesAPartialMarker(t *testing.T) {
	in := []byte("out\x1b]133;D;7\a\x1b]7717;backend=gpu03;ended=aiot\a\x1b]7717;replayed\a")
	for cut := 0; cut <= len(in); cut++ {
		var p Parser
		evs := append(p.Feed(in[:cut]), p.Feed(in[cut:])...)
		var text string
		var rest []Event
		for _, e := range evs {
			if e.Kind == Text {
				text += string(e.Data)
			} else {
				rest = append(rest, e)
			}
		}
		if text != "out" || len(rest) != 3 || rest[0].Code != 7 ||
			rest[1].Backend != "gpu03" || rest[1].Ended != "aiot" ||
			rest[2].Kind != Replayed {
			t.Fatalf("cut %d: text %q, events %+v", cut, text, rest)
		}
	}
}

// Other programs' OSCs, and other escapes, are theirs.
func TestParserLeavesOtherEscapesAlone(t *testing.T) {
	var p Parser
	in := "\x1b]0;title\a\x1b[?1049h\x1b]8;;http://x\x1b\\link\x1b]8;;\x1b\\"
	evs := p.Feed([]byte(in))
	if len(evs) != 1 || string(evs[0].Data) != in {
		t.Fatalf("got %+v", evs)
	}
}

func TestParserGivesUpOnARunawayOSC(t *testing.T) {
	var p Parser
	big := make([]byte, maxOSC+10)
	for i := range big {
		big[i] = 'x'
	}
	in := append([]byte("\x1b]"), big...)
	evs := p.Feed(in)
	evs = append(evs, p.Feed([]byte("\x1b]133;C\a"))...)
	if len(evs) < 2 || evs[len(evs)-1].Kind != Exec {
		t.Fatalf("got %d events, last %+v", len(evs), evs[len(evs)-1])
	}
}
