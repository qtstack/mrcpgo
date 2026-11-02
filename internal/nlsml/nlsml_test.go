package nlsml

import (
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	doc := string(Build(Result{Text: "hello world", Confidence: 95, Instance: "aliyun"}))
	for _, want := range []string{
		"<?xml version=\"1.0\"?>",
		"<result>",
		"<interpretation confidence=\"95\">",
		"<input mode=\"speech\">hello world</input>",
		"<instance>aliyun</instance>",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("document missing %q:\n%s", want, doc)
		}
	}
}

func TestBuildEscapesText(t *testing.T) {
	doc := string(Build(Result{Text: `a<b> & "c"`, Confidence: 50}))
	if !strings.Contains(doc, "a&lt;b&gt; &amp; &#34;c&#34;") {
		t.Fatalf("text was not escaped:\n%s", doc)
	}
	if strings.Contains(doc, "<b>") {
		t.Fatalf("raw markup leaked into the document:\n%s", doc)
	}
}

func TestBuildClampsConfidence(t *testing.T) {
	if doc := string(Build(Result{Text: "x", Confidence: 250})); !strings.Contains(doc, `confidence="100"`) {
		t.Errorf("high confidence not clamped:\n%s", doc)
	}
	if doc := string(Build(Result{Text: "x", Confidence: 0})); !strings.Contains(doc, `confidence="0"`) {
		t.Errorf("zero confidence missing:\n%s", doc)
	}
}

func TestBuildOmitsNegativeConfidence(t *testing.T) {
	doc := string(Build(Result{Text: "x", Confidence: -1}))
	if strings.Contains(doc, "confidence=") {
		t.Errorf("confidence attribute should be omitted:\n%s", doc)
	}
}

func TestBuildWithoutInstance(t *testing.T) {
	doc := string(Build(Result{Text: "x", Confidence: 1}))
	if strings.Contains(doc, "<instance>") {
		t.Errorf("unexpected instance element:\n%s", doc)
	}
}
