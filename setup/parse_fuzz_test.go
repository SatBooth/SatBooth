package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// stateSeeds is a state file of every stage and host.
func stateSeeds() map[Stage][]byte {
	pa := baseAnswers()
	pubInit := &state{Version: 1, Host: hostPublic, Stage: StagePublicInit, Public: &pa, HubURL: "http://127.0.0.1:8080",
		InvoiceClient: curveKeys[0]}
	pr := baseAnswers()
	withRefunds(&pr, 2, 100000)
	pubDone := &state{Version: 1, Host: hostPublic, Stage: StagePublicDone, Public: &pr, HubURL: "http://127.0.0.1:8080",
		InvoiceClient: curveKeys[0], PayoutClient: curveKeys[1]}
	ha := baseHub()
	in := &PublicToHub{Version: 1, Network: "mainnet", HubPrivateAddr: "192.0.2.20", PublicPrivateAddr: "192.0.2.10",
		InvoiceClient: curveKeys[0], PayoutClient: curveKeys[1]}
	hubInit := &state{Version: 1, Host: hostHub, Stage: StageHubInit, Hub: &ha, Handoff: in}
	hubPair := *hubInit
	hubPair.Stage = StageHubPair
	return map[Stage][]byte{StagePublicInit: pubInit.marshal(), StagePublicDone: pubDone.marshal(),
		StageHubInit: hubInit.marshal(), StageHubPair: hubPair.marshal()}
}

// journalSeeds is a journal of every stage, as the stage would write it.
func journalSeeds() map[Stage][]byte {
	states := stateSeeds()
	out := map[Stage][]byte{}
	for stage, sp := range specs {
		j := journal{Version: 1, Stage: stage, Dirs: append(append([]string{}, sp.dirs...), tmpDirName(stage)), Files: []string{}, SHA256: []string{}}
		for _, f := range sp.files {
			j.Files = append(j.Files, f)
			j.SHA256 = append(j.SHA256, sumHex([]byte(f)))
		}
		if !sp.first() {
			p := string(states[sp.prev])
			j.PrevState = &p
		}
		b, err := json.MarshalIndent(&j, "", "  ")
		if err != nil {
			panic(err)
		}
		out[stage] = b
	}
	return out
}

// mutations are the texts a strict parser must refuse, made from a valid one: key is the first key of the object.
func mutations(valid []byte) map[string][]byte {
	s := string(valid)
	first := `"version": 1,`
	m := map[string][]byte{
		"a trailing }":         append(append([]byte{}, valid...), '}'),
		"a trailing ]":         append(append([]byte{}, valid...), ']'),
		"a second object":      append(append([]byte{}, valid...), []byte("{}")...),
		"a duplicate key":      []byte(strings.Replace(s, first, first+"\n  "+first, 1)),
		"an escaped duplicate": []byte(strings.Replace(s, first, first+"\n  \"vers\\u0069on\": 1,", 1)),
		"a case-variant key":   []byte(strings.Replace(s, `"version"`, `"Version"`, 1)),
		"an unknown key":       []byte(strings.Replace(s, "{", "{\n  \"extra\": 1,", 1)),
		"a wrong type":         []byte(strings.Replace(s, first, `"version": "1",`, 1)),
		"null":                 []byte("null"),
		"an array":             []byte("[]"),
		"empty":                nil,
		"a BOM":                append([]byte("\xef\xbb\xbf"), valid...),
		"invalid UTF-8":        []byte(strings.Replace(s, `"stage": "`, "\"stage\": \"\xff", 1)),
		"oversize":             append(append([]byte{}, valid...), bytes.Repeat([]byte(" "), maxState)...),
		"a null value":         []byte(strings.Replace(s, first, `"version": null,`, 1)),
	}
	return m
}

func TestStateAndJournalParsersAreStrict(t *testing.T) {
	for name, seeds := range map[string]map[Stage][]byte{"state": stateSeeds(), "journal": journalSeeds()} {
		parse := func(b []byte) error {
			if name == "state" {
				_, err := parseState(b)
				return err
			}
			_, err := parseJournal(b)
			return err
		}
		for stage, valid := range seeds {
			if err := parse(valid); err != nil {
				t.Fatalf("%s %s: the seed is refused: %v", name, stage, err)
			}
			if err := parse(append(append([]byte{}, valid...), " \n\t"...)); err != nil { // white space after is fine
				t.Fatalf("%s %s: trailing white space: %v", name, stage, err)
			}
			for what, bad := range mutations(valid) {
				if err := parse(bad); err == nil {
					t.Errorf("%s %s: %s was accepted", name, stage, what)
				}
			}
		}
	}
}

func FuzzParseState(f *testing.F) {
	for _, valid := range stateSeeds() {
		f.Add(valid)
		for _, bad := range mutations(valid) {
			f.Add(bad)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		st, err := parseState(b)
		if err != nil {
			return
		}
		again, err := parseState(st.marshal())
		if err != nil || !reflect.DeepEqual(st, again) {
			t.Fatalf("re-encoding changed the state: %v", err)
		}
	})
}

func FuzzParseJournal(f *testing.F) {
	for _, valid := range journalSeeds() {
		f.Add(valid)
		for _, bad := range mutations(valid) {
			f.Add(bad)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		j, err := parseJournal(b)
		if err != nil {
			return
		}
		sp := specs[j.Stage]
		for _, n := range j.Dirs {
			if !sp.allowsDir(j.Stage, n) {
				t.Fatalf("a directory outside the stage's set: %q", n)
			}
		}
		for i, n := range j.Files {
			if !sp.allowsFile(j.Stage, n) || !isHex(j.SHA256[i], 64) {
				t.Fatalf("a file outside the stage's set: %q", n)
			}
		}
		re, err := json.MarshalIndent(j, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		again, err := parseJournal(re)
		if err != nil || !reflect.DeepEqual(j, again) {
			t.Fatalf("re-encoding changed the journal: %v", err)
		}
	})
}

// The writer always journals setup/state.json.tmp: a journal without it is one the writer could not have produced.
func TestParseJournalRequiresTheStateScratchName(t *testing.T) {
	for stage, valid := range journalSeeds() {
		var j journal
		if err := json.Unmarshal(valid, &j); err != nil {
			t.Fatal(err)
		}
		drop := func(keep func(name string) bool) []byte {
			c := j
			c.Files, c.SHA256 = nil, nil
			for i, n := range j.Files {
				if keep(n) {
					c.Files, c.SHA256 = append(c.Files, n), append(c.SHA256, j.SHA256[i])
				}
			}
			if c.Files == nil {
				c.Files, c.SHA256 = []string{}, []string{}
			}
			b, err := json.MarshalIndent(&c, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		if !slices.Contains(j.Files, stateTmp) {
			t.Fatalf("%s: the seed does not list %s", stage, stateTmp)
		}
		if _, err := parseJournal(drop(func(string) bool { return true })); err != nil {
			t.Errorf("%s: the whole journal is refused: %v", stage, err)
		}
		if _, err := parseJournal(drop(func(n string) bool { return n != stateTmp })); !errors.Is(err, errBadJournal) {
			t.Errorf("%s: a journal without %s: %v", stage, stateTmp, err)
		}
		if _, err := parseJournal(drop(func(string) bool { return false })); !errors.Is(err, errBadJournal) {
			t.Errorf("%s: a journal with no files: %v", stage, err)
		}
		if _, err := parseJournal(drop(func(n string) bool { return n == stateTmp })); err != nil {
			t.Errorf("%s: a journal of only %s is refused: %v", stage, stateTmp, err)
		}
	}
}
