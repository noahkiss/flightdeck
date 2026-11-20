package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/noahkiss/flightdeck/api"
	"github.com/noahkiss/flightdeck/internal/detect"
)

// ToolQuestion is Claude Code's question tool. Its permission request is answered with text,
// not allowed or denied (harness-adapters.md, Question answers).
const ToolQuestion = "AskUserQuestion"

// MaxAnswerText bounds the text of a question's answer, in bytes; a longer one is refused.
const MaxAnswerText = 16 << 10

// MaxHookAnswer bounds the body a waiting hook takes from the hab, in bytes. The hook reads no
// more, so the hab refuses an answer whose body would be longer.
const MaxHookAnswer = 256 << 10

// Question is one question of an AskUserQuestion request.
type Question struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// QuestionAnswer is the owner's answer to an AskUserQuestion request, ready for the harness.
type QuestionAnswer struct {
	// Input is the request's own tool input with "answers" added: the hook's updatedInput.
	Input json.RawMessage
	// Questions are the request's questions, in order.
	Questions []Question
	// Answers holds each question's answer, by question text.
	Answers map[string]string
}

// AnswerQuestion maps the text of an approval.answer to the questions of the open request.
// Only a Claude Code AskUserQuestion request with its input takes an answer. With one question
// the whole text is its answer: an option's label, labels joined with ", ", or free text. With
// several, each line reads "<header or question>: <answer>", one per question, every question
// answered once, as the app writes them. Anything else is refused and nothing is sent.
func AnswerQuestion(st api.AgentState, text string) (QuestionAnswer, error) {
	if st.Harness != detect.KindClaude {
		return QuestionAnswer{}, fmt.Errorf("%w: %s sends no questions the hab can answer; answer it in the terminal", ErrUnsupported, st.Harness)
	}
	r := st.Request
	if r == nil || r.Tool != ToolQuestion {
		return QuestionAnswer{}, errors.New("the open request is a permission, not a question: answer it with allow or deny")
	}
	if len(text) > MaxAnswerText {
		return QuestionAnswer{}, fmt.Errorf("the answer is %d bytes; the most is %d", len(text), MaxAnswerText)
	}
	text = strings.TrimSpace(stripControls(text))
	if text == "" {
		return QuestionAnswer{}, errors.New("the answer is empty")
	}
	if r.InputTruncated || len(r.Input) == 0 {
		return QuestionAnswer{}, fmt.Errorf("%w: the hab does not hold the question's input; answer it in the terminal", ErrUnsupported)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Input, &fields); err != nil {
		return QuestionAnswer{}, fmt.Errorf("%w: the question's input does not read as an object (%v); answer it in the terminal", ErrUnsupported, err)
	}
	if _, ok := fields["answers"]; ok {
		return QuestionAnswer{}, fmt.Errorf("%w: the question already carries answers; answer it in the terminal", ErrUnsupported)
	}
	var qs []Question
	if err := json.Unmarshal(fields["questions"], &qs); err != nil || len(qs) == 0 {
		return QuestionAnswer{}, fmt.Errorf("%w: the request holds no questions the hab can read; answer it in the terminal", ErrUnsupported)
	}
	answers := map[string]string{}
	if len(qs) == 1 {
		answers[qs[0].Question] = text
	} else {
		var err error
		if answers, err = lineAnswers(qs, text); err != nil {
			return QuestionAnswer{}, err
		}
	}
	b, err := MarshalPlain(answers)
	if err != nil {
		return QuestionAnswer{}, err
	}
	fields["answers"] = b
	in, err := MarshalPlain(fields)
	if err != nil {
		return QuestionAnswer{}, err
	}
	// The hook body wraps the input: {"behavior":"allow","updatedInput":<input>}.
	if n := len(in) + hookWrap; n > MaxHookAnswer {
		return QuestionAnswer{}, fmt.Errorf("%w: the answered question is %d bytes, more than the hook takes (%d); answer it in the terminal", ErrUnsupported, n, MaxHookAnswer)
	}
	return QuestionAnswer{Input: in, Questions: qs, Answers: answers}, nil
}

// hookWrap is room for the hook body around a question's input.
const hookWrap = 64

// MarshalPlain is json.Marshal without HTML escaping: <, > and & stay one byte each, so a
// body does not grow on its way to the hook. The result has no trailing newline.
func MarshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// stripControls drops the control characters of C0 (all but newline and tab), DEL and C1 from
// an answer: Claude Code echoes answers to its terminal, where one could start an escape.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, s)
}

// lineAnswers reads one "<header or question>: <answer>" line per question. A line names the
// question whose header or text, followed by a colon, starts it; the longest such name wins,
// and a name two questions share fits neither.
func lineAnswers(qs []Question, text string) (map[string]string, error) {
	want := fmt.Errorf("the request asks %d questions: answer each on its own line as \"<header>: <answer>\"", len(qs))
	answers := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		best, n, tie := -1, 0, false
		for i, q := range qs {
			for _, name := range []string{q.Header, q.Question} {
				name = strings.TrimSpace(name)
				if name == "" || !strings.HasPrefix(line, name+":") {
					continue
				}
				switch {
				case len(name) > n:
					best, n, tie = i, len(name), false
				case len(name) == n && i != best:
					tie = true
				}
			}
		}
		if best < 0 || tie {
			return nil, want
		}
		q := qs[best].Question
		a := strings.TrimSpace(line[n+1:])
		if _, dup := answers[q]; dup || a == "" {
			return nil, want
		}
		answers[q] = a
	}
	if len(answers) != len(qs) {
		return nil, want
	}
	return answers, nil
}

// QuestionKey returns the key that picks the answer in Claude Code's question dialog. The dialog
// takes a digit as pick and submit at once for one question with a single choice (checked
// against Claude Code 2.1.329). So only that case goes by keys, and only when the answer is one
// option's label, exactly. The digit is the label's position among the options. It is sent only
// when the screen shows this question's dialog (questionDialog) with the cursor on an option
// row: on the "Type something." row a digit is typed as text. Free text, several questions,
// multiple choice, or no such dialog on screen report false: nothing is sent.
func QuestionKey(a QuestionAnswer, rows []string) ([]byte, bool) {
	if len(a.Questions) != 1 || a.Questions[0].MultiSelect {
		return nil, false
	}
	q := a.Questions[0]
	text := a.Answers[q.Question]
	pick := -1
	for i, o := range q.Options {
		if strings.TrimSpace(o.Label) != text {
			continue
		}
		if pick >= 0 {
			return nil, false // two options carry the label
		}
		pick = i
	}
	if pick < 0 {
		return nil, false
	}
	opts, ok := questionDialog(q, rows)
	if !ok || !cursorOnOption(rows, opts) {
		return nil, false
	}
	return []byte(strconv.Itoa(pick + 1)), true
}

// cursorOnOption reports whether the dialog's cursor (❯) is on one of its option rows.
func cursorOnOption(rows []string, opts []int) bool {
	for _, i := range opts {
		if strings.Contains(rows[i], "❯") {
			return true
		}
	}
	return false
}

// questionDialog finds the last block on screen that is the question's dialog, and returns the
// row index of each option. A block is numbered rows 1 to n that carry the options' labels in
// order, with other rows (descriptions) between them; row n+1 reading "Type something."; and
// the question's text above row 1, below any rule. A numbered list in Claude's prose above the
// dialog is no such block, and a screen where the dialog is not drawn yet has none.
func questionDialog(q Question, rows []string) ([]int, bool) {
	n := len(q.Options)
	if n == 0 || n+1 > 9 {
		return nil, false // the dialog numbers its rows with one digit
	}
	free := strconv.Itoa(n + 1)
	for end := len(rows) - 1; end >= 0; end-- {
		m := optionLine.FindStringSubmatch(rows[end])
		if m == nil || m[1] != free || strings.TrimSpace(m[2]) != "Type something." {
			continue
		}
		opts := make([]int, n)
		want := n
		for i := end - 1; i >= 0 && want > 0; i-- {
			m := optionLine.FindStringSubmatch(rows[i])
			if m == nil {
				continue // a description row
			}
			if m[1] != strconv.Itoa(want) || strings.TrimSpace(m[2]) != strings.TrimSpace(q.Options[want-1].Label) {
				break
			}
			want--
			opts[want] = i
		}
		if want == 0 && asksAbove(q.Question, rows[:opts[0]]) {
			return opts, true
		}
	}
	return nil, false
}

// asksAbove reports whether the rows above a dialog's first option, up to the nearest rule,
// show the question's text. The dialog wraps a long question and starts each of its rows with
// "│" (Claude Code 2.1.329), so white space and "│" are ignored.
func asksAbove(question string, rows []string) bool {
	squash := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) || r == '│' {
				return -1
			}
			return r
		}, s)
	}
	want := squash(question)
	if want == "" {
		return false
	}
	from := 0
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.Contains(rows[i], "────") {
			from = i + 1
			break
		}
	}
	return strings.Contains(squash(strings.Join(rows[from:], "")), want)
}
