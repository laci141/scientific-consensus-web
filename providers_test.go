package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// consensusJSON builds a consensus-shaped CLI output with n all_studies entries
// (each carrying an abstract) plus top_supporting/top_refuting lists.
func consensusJSON(t *testing.T, n int) []byte {
	t.Helper()
	studies := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		studies = append(studies, map[string]any{
			"title":    fmt.Sprintf("Study %d", i),
			"year":     2020,
			"abstract": fmt.Sprintf("Abstract of study %d.", i),
		})
	}
	out, err := json.Marshal(map[string]any{
		"claim":           "vitamin D reduces respiratory infections",
		"verdict":         "supported",
		"consensus_score": 0.8,
		"top_supporting":  studies[:min(2, n)],
		"top_refuting":    []map[string]any{},
		"all_studies":     studies,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// decode unmarshals compacted output for assertions.
func decode(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("compacted output is not valid JSON: %v", err)
	}
	return obj
}

func studyCount(t *testing.T, obj map[string]json.RawMessage) int {
	t.Helper()
	var list []json.RawMessage
	if err := json.Unmarshal(obj["all_studies"], &list); err != nil {
		t.Fatalf("all_studies not an array: %v", err)
	}
	return len(list)
}

func TestCompactForLLMCapsAllStudiesAndDropsTopLists(t *testing.T) {
	got := decode(t, compactForLLM(consensusJSON(t, 40)))

	if n := studyCount(t, got); n != maxStudiesForLLM {
		t.Errorf("all_studies: got %d entries, want %d", n, maxStudiesForLLM)
	}
	for _, k := range []string{"top_supporting", "top_refuting"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s should be removed when all_studies is present", k)
		}
	}
	// The trim keeps the FIRST (most relevant) entries and their abstracts.
	var list []struct {
		Title    string `json:"title"`
		Abstract string `json:"abstract"`
	}
	if err := json.Unmarshal(got["all_studies"], &list); err != nil {
		t.Fatal(err)
	}
	if list[0].Title != "Study 0" || list[len(list)-1].Title != fmt.Sprintf("Study %d", maxStudiesForLLM-1) {
		t.Errorf("trim did not keep the first %d entries: first=%q last=%q", maxStudiesForLLM, list[0].Title, list[len(list)-1].Title)
	}
	if list[0].Abstract == "" {
		t.Error("abstract lost during compaction")
	}
	// Unrelated fields survive.
	if _, ok := got["consensus_score"]; !ok {
		t.Error("consensus_score dropped during compaction")
	}
}

func TestCompactForLLMShortListKeptButTopListsStillDropped(t *testing.T) {
	got := decode(t, compactForLLM(consensusJSON(t, 10)))
	if n := studyCount(t, got); n != 10 {
		t.Errorf("all_studies: got %d entries, want 10 (no trim below cap)", n)
	}
	if _, ok := got["top_supporting"]; ok {
		t.Error("top_supporting should be removed even when all_studies is under the cap")
	}
}

func TestCompactForLLMCompareNested(t *testing.T) {
	cmp, err := json.Marshal(map[string]any{
		"claim_a":          json.RawMessage(consensusJSON(t, 30)),
		"claim_b":          json.RawMessage(consensusJSON(t, 30)),
		"stronger_support": "claim_a",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := decode(t, compactForLLM(cmp))
	for _, k := range []string{"claim_a", "claim_b"} {
		sub := decode(t, got[k])
		if n := studyCount(t, sub); n != maxStudiesForLLM {
			t.Errorf("%s.all_studies: got %d entries, want %d", k, n, maxStudiesForLLM)
		}
		if _, ok := sub["top_supporting"]; ok {
			t.Errorf("%s.top_supporting should be removed", k)
		}
	}
	var stronger string
	if err := json.Unmarshal(got["stronger_support"], &stronger); err != nil || stronger != "claim_a" {
		t.Errorf("stronger_support corrupted: %s err=%v", got["stronger_support"], err)
	}
}

func TestCompactForLLMNoAllStudiesUnchanged(t *testing.T) {
	// evidence/gaps/controversies-shaped output (or an older CLI binary).
	raw := []byte(`{"claim":"x","designs":[{"design":"rct","count":3}],"note":"n"}`)
	if got := compactForLLM(raw); !bytes.Equal(got, raw) {
		t.Errorf("JSON without all_studies must be returned byte-identical\ngot:  %s\nwant: %s", got, raw)
	}
}

func TestCompactForLLMInvalidInputsUnchanged(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`not json at all`),
		[]byte(`[1,2,3]`),                  // top level not an object
		[]byte(`{"all_studies":"oops"}`),   // all_studies not an array
		[]byte(`{"claim_a":"not-object"}`), // compare key not an object
	} {
		if got := compactForLLM(raw); !bytes.Equal(got, raw) {
			t.Errorf("input %q must pass through unchanged, got %q", raw, got)
		}
	}
}

func TestSynthesisPromptUsesCompactedJSON(t *testing.T) {
	prompt := synthesisPrompt("consensus", []string{"claim"}, consensusJSON(t, 40))
	if strings.Contains(prompt, "top_supporting") {
		t.Error("prompt still contains top_supporting after compaction")
	}
	if !strings.Contains(prompt, "all_studies") {
		t.Error("prompt does not contain all_studies")
	}
	if !strings.Contains(prompt, fmt.Sprintf("Study %d", maxStudiesForLLM-1)) {
		t.Errorf("prompt missing study %d (last kept entry)", maxStudiesForLLM-1)
	}
	if strings.Contains(prompt, fmt.Sprintf(`"Study %d"`, maxStudiesForLLM)) {
		t.Errorf("prompt contains study %d, which should be trimmed", maxStudiesForLLM)
	}
}
