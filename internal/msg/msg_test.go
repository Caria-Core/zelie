package msg

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestWith(t *testing.T) {
	tm := Template{Code: "test.with", English: "{name} holds {n} files; {n} is a lot. {missing} stays.", Status: http.StatusConflict}
	m := tm.With("name", "/data", "n", 3)
	if m.Text != "/data holds 3 files; 3 is a lot. {missing} stays." || m.Params["n"] != 3 || m.Code != "test.with" {
		t.Errorf("%+v", m)
	}
	if got := tm.Names(); fmt.Sprint(got) != "[name n missing]" {
		t.Errorf("names %v", got)
	}
	e := tm.Err("name", "x", "n", 1)
	if e.Status != http.StatusConflict || e.Error() != e.Text {
		t.Errorf("%+v", e)
	}
	if e.WithStatus(http.StatusNotFound).Status != http.StatusNotFound {
		t.Error("WithStatus did not change the status")
	}
}

func TestWrap(t *testing.T) {
	tm := Template{Code: "test.wrap", English: "Own words."}
	if m := Wrap(fmt.Errorf("context: %w", tm.Err())); m.Code != "test.wrap" {
		t.Errorf("wrapped message lost: %+v", m)
	}
	if m := Wrap(errors.New("exit status 1")); m.Code != Other.Code || m.Text != "exit status 1" || m.Params["detail"] != "exit status 1" {
		t.Errorf("other: %+v", m)
	}
}

func TestDefineRefuses(t *testing.T) {
	for _, code := range []string{"nodot", "Upper.case", "x.", ".x", "a.b-c", "other.detail"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%q was accepted", code)
				}
			}()
			Define(0, code, "x")
		}()
	}
}
