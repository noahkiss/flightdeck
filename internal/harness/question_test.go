package harness

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/noahkiss/flightdeck/api"
)

// Hand-written AskUserQuestion inputs in the shape Claude Code 2.1.329 sends in its
// PermissionRequest hook.
const (
	oneQuestion  = `{"questions":[{"question":"Which color should the status badge use?","header":"Color","options":[{"label":"Red","description":"Matches the error banner."},{"label":"Blue","description":"Matches the link color."}],"multiSelect":false}]}`
	twoQuestions = `{"questions":[{"question":"Which color should the status badge use?","header":"Color","options":[{"label":"Red"},{"label":"Blue"}],"multiSelect":false},{"question":"Which sizes should it come in?","header":"Size","options":[{"label":"Small"},{"label":"Large"}],"multiSelect":true}]}`
)

func questionState(harness, input string) api.AgentState {
	return api.AgentState{Harness: harness, Request: &api.PermissionRequest{ID: "r", Tool: ToolQuestion, Input: api.RawJSON(input)}}
}

// TestAnswerQuestion: the answer text becomes the question's answers, keyed by question text,
// added to the request's own input with nothing else changed. Several questions take one
// "<header or question>: <answer>" line each. Other harnesses, permissions and inputs the hab
// cannot read are refused.
func TestAnswerQuestion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input, text string
		want              map[string]string
	}{
		{"an option", oneQuestion, "Blue", map[string]string{"Which color should the status badge use?": "Blue"}},
		{"free text, trimmed", oneQuestion, "  Green, but darker \n", map[string]string{"Which color should the status badge use?": "Green, but darker"}},
		{"two questions by header", twoQuestions, "Color: Blue\nSize: Small, Large", map[string]string{"Which color should the status badge use?": "Blue", "Which sizes should it come in?": "Small, Large"}},
		{"two questions by question, any order", twoQuestions, "Which sizes should it come in?: Large\n\nColor: Red", map[string]string{"Which color should the status badge use?": "Red", "Which sizes should it come in?": "Large"}},
	} {
		qa, err := AnswerQuestion(questionState("claude", tc.input), tc.text)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(qa.Answers, tc.want) {
			t.Errorf("%s: answers %v", tc.name, qa.Answers)
		}
		var got, orig map[string]any
		if err := json.Unmarshal(qa.Input, &got); err != nil {
			t.Fatal(err)
		}
		json.Unmarshal([]byte(tc.input), &orig)
		answers := got["answers"]
		delete(got, "answers")
		if !reflect.DeepEqual(got, orig) {
			t.Errorf("%s: input changed: %s", tc.name, qa.Input)
		}
		b, _ := json.Marshal(answers)
		w, _ := json.Marshal(tc.want)
		if string(b) != string(w) {
			t.Errorf("%s: input answers %s", tc.name, b)
		}
	}

	for _, tc := range []struct {
		name string
		st   api.AgentState
		text string
		kind error // nil: a plain refusal, which the API answers invalid
	}{
		{"codex", questionState("codex", oneQuestion), "Blue", ErrUnsupported},
		{"opencode", questionState("opencode", oneQuestion), "Blue", ErrUnsupported},
		{"a permission", api.AgentState{Harness: "claude", Request: &api.PermissionRequest{ID: "r", Tool: "Bash", Command: "make"}}, "Blue", nil},
		{"no request", api.AgentState{Harness: "claude"}, "Blue", nil},
		{"empty", questionState("claude", oneQuestion), " \n", nil},
		{"input truncated", api.AgentState{Harness: "claude", Request: &api.PermissionRequest{ID: "r", Tool: ToolQuestion, InputTruncated: true}}, "Blue", ErrUnsupported},
		{"no questions", questionState("claude", `{"questions":[]}`), "Blue", ErrUnsupported},
		{"answers already there", questionState("claude", strings.Replace(oneQuestion, `{"questions"`, `{"answers":{},"questions"`, 1)), "Blue", ErrUnsupported},
		{"two questions, free text", questionState("claude", twoQuestions), "Blue and small", nil},
		{"two questions, one answered", questionState("claude", twoQuestions), "Color: Blue", nil},
		{"two questions, one twice", questionState("claude", twoQuestions), "Color: Blue\nColor: Red\nSize: Small", nil},
		{"two questions, an empty answer", questionState("claude", twoQuestions), "Color: Blue\nSize:", nil},
		{"a header two questions share", questionState("claude", strings.Replace(twoQuestions, `"Size"`, `"Color"`, 1)), "Color: Blue\nColor: Small", nil},
	} {
		_, err := AnswerQuestion(tc.st, tc.text)
		if err == nil {
			t.Errorf("%s: answered", tc.name)
			continue
		}
		if tc.kind != nil && !errors.Is(err, tc.kind) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.kind)
		}
		if tc.kind == nil && errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want a plain refusal", tc.name, err)
		}
	}
}

// TestQuestionKey: one question with a single choice is picked by the digit of the row that
// carries the answer's label, read from the hand-written dialog of Claude Code 2.1.329. Free
// text, a label not among the options, several questions, multiple choice, a screen without the
// row, or a label on two rows send nothing.
func TestQuestionKey(t *testing.T) {
	t.Parallel()
	d, err := os.ReadFile("testdata/claude-question-dialog-2.1.329.txt")
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Split(string(d), "\n")
	answer := func(input, text string) QuestionAnswer {
		t.Helper()
		qa, err := AnswerQuestion(questionState("claude", input), text)
		if err != nil {
			t.Fatal(err)
		}
		return qa
	}
	if k, ok := QuestionKey(answer(oneQuestion, "Blue"), rows); !ok || string(k) != "2" {
		t.Errorf("Blue: %q %v", k, ok)
	}
	if k, ok := QuestionKey(answer(oneQuestion, "Red"), rows); !ok || string(k) != "1" {
		t.Errorf("Red, under the cursor: %q %v", k, ok)
	}
	for name, qa := range map[string]QuestionAnswer{
		"free text":              answer(oneQuestion, "Green, but darker"),
		"the dialog's own row":   answer(oneQuestion, "Type something."),
		"two questions":          answer(twoQuestions, "Color: Blue\nSize: Small"),
		"multiple choice":        answer(strings.Replace(oneQuestion, `"multiSelect":false`, `"multiSelect":true`, 1), "Blue"),
		"no questions to choose": {},
	} {
		if k, ok := QuestionKey(qa, rows); ok {
			t.Errorf("%s: key %q", name, k)
		}
	}
	if k, ok := QuestionKey(answer(oneQuestion, "Blue"), []string{"$ "}); ok {
		t.Errorf("no dialog: key %q", k)
	}
	twice := append(append([]string{}, rows...), "  5. Blue")
	if k, ok := QuestionKey(answer(oneQuestion, "Blue"), twice); ok {
		t.Errorf("label on two rows: key %q", k)
	}
}
