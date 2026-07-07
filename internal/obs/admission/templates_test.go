package admission

import "testing"

// TestStarterTemplatesCompileAndLint pins the §7 acceptance: every starter
// template, once its purpose/topic placeholders are filled, produces a
// criterion that Compile accepts and Lint passes with no fatal issue.
func TestStarterTemplatesCompileAndLint(t *testing.T) {
	for _, tpl := range StarterTemplates() {
		t.Run(tpl.Key, func(t *testing.T) {
			c, ok := tpl.Render("scheduling assistant for Acme's booking app", []string{"Competitor Inc", "pricing"})
			if !ok {
				t.Fatalf("Render returned ok=false with inputs supplied")
			}
			if c.ID == "" || c.Type == "" {
				t.Fatalf("rendered criterion missing id/type: %+v", c)
			}
			if tpl.NeedsPurpose && c.Definition == "" {
				t.Errorf("valid_use_case template rendered an empty definition")
			}
			if tpl.NeedsTopics && len(c.Topics) == 0 {
				t.Errorf("denied_topics template rendered no topics")
			}
			in := PolicyInput{Criteria: []CriterionInput{c}}
			if issues := Lint(in); HasFatal(issues) {
				t.Errorf("template %s lints fatal: %+v", tpl.Key, issues)
			}
			if _, err := Compile(in); err != nil {
				t.Errorf("template %s failed to compile: %v", tpl.Key, err)
			}
		})
	}
}

// TestTemplateRenderSkipsWithoutInput pins that a purpose/topic-needing
// template skips (ok=false) rather than emitting a lint-fatal criterion when
// the required input is absent.
func TestTemplateRenderSkipsWithoutInput(t *testing.T) {
	onScope, ok := TemplateByKey("on_scope")
	if !ok {
		t.Fatal("on_scope template missing")
	}
	if _, ok := onScope.Render("   ", nil); ok {
		t.Errorf("on_scope rendered ok with a blank purpose")
	}
	topics, ok := TemplateByKey("denied_topics")
	if !ok {
		t.Fatal("denied_topics template missing")
	}
	if _, ok := topics.Render("", []string{"   "}); ok {
		t.Errorf("denied_topics rendered ok with only-blank topics")
	}
	// The jailbreak template needs no input.
	jb, ok := TemplateByKey("jailbreak")
	if !ok {
		t.Fatal("jailbreak template missing")
	}
	if _, ok := jb.Render("", nil); !ok {
		t.Errorf("jailbreak should render without input")
	}
}
