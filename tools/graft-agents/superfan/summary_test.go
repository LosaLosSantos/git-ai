package superfan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNoneSummarizerIsNil(t *testing.T) {
	for _, typ := range []string{"", "none"} {
		s, err := NewSummarizer(typ, nil)
		if err != nil || s != nil {
			t.Errorf("NewSummarizer(%q) = %v, %v; want nil, nil", typ, s, err)
		}
	}
}

func TestNewSummarizerUnknownType(t *testing.T) {
	if _, err := NewSummarizer("oracle", nil); err == nil {
		t.Error("unknown type accepted")
	}
}

func TestRegisterSummarizer(t *testing.T) {
	const name = "test-register-ok"
	var gotOptions json.RawMessage
	RegisterSummarizer(name, func(opts json.RawMessage) (Summarizer, error) {
		gotOptions = opts
		return &fakeSummarizer{sum: Summary{Text: "custom"}}, nil
	})
	if !isSummarizerRegistered(name) {
		t.Fatal("registered summarizer not visible")
	}
	found := false
	for _, n := range SummarizerTypes() {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("SummarizerTypes() lacks %q", name)
	}
	s, err := NewSummarizer(name, json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotOptions) != `{"model":"m"}` {
		t.Errorf("options = %s", gotOptions)
	}
	sum, _ := s.Summarize(context.Background(), Digest{})
	if sum.Text != "custom" {
		t.Errorf("summary = %q", sum.Text)
	}
}

func TestRegisterSummarizerPanics(t *testing.T) {
	assertPanic := func(name string, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s did not panic", name)
			}
		}()
		f()
	}
	factory := func(json.RawMessage) (Summarizer, error) { return nil, nil }
	assertPanic("empty name", func() { RegisterSummarizer("", factory) })
	assertPanic("nil factory", func() { RegisterSummarizer("test-nil-factory", nil) })
	assertPanic("duplicate", func() { RegisterSummarizer("none", factory) })
}

func TestNewSummarizerFactoryError(t *testing.T) {
	const name = "test-factory-error"
	RegisterSummarizer(name, func(json.RawMessage) (Summarizer, error) {
		return nil, errors.New("no key")
	})
	if _, err := NewSummarizer(name, nil); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("factory error not returned: %v", err)
	}
}

func TestRiskRanks(t *testing.T) {
	if riskRank("unknown") != 0 || riskRank(SeverityHigh) != 3 {
		t.Error("riskRank wrong")
	}
}
