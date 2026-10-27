package fakehub

import (
	"slices"
	"testing"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash/transport"
)

func TestDropRequests_IgnoresTheNextNUnread(t *testing.T) {
	h := New(t, func(transport.Item) *transport.Result { return nil })
	h.DropRequests(1)
	ev := &nip01.Event{Tags: [][]string{{"p", h.Inbox()}}}
	h.handleRequest(ev)
	if n := h.RequestsTo(h.Inbox()); n != 0 {
		t.Errorf("%d requests recorded while dropping; want 0", n)
	}
	h.handleRequest(ev)
	if n := h.RequestsTo(h.Inbox()); n != 1 {
		t.Errorf("%d requests recorded after the drop; want 1", n)
	}
}

func TestDropReplies_CountsPerMethod(t *testing.T) {
	h := New(t, func(transport.Item) *transport.Result { return nil })
	h.DropReplies("cash_transfer", 2)
	transfer := []transport.Item{{Method: "cash_status"}, {Method: "cash_transfer"}}
	status := []transport.Item{{Method: "cash_status"}}
	got := []bool{h.takeDrop(status), h.takeDrop(transfer), h.takeDrop(transfer), h.takeDrop(transfer)}
	if want := []bool{false, true, true, false}; !slices.Equal(got, want) {
		t.Errorf("drops %v, want %v", got, want)
	}
	h.DropReplies("", -1)
	for range 3 {
		if !h.takeDrop(status) {
			t.Fatal("DropReplies(\"\", -1) let a reply through")
		}
	}
}

func TestAnswer_CodeAndRandomMessages(t *testing.T) {
	h := New(t, func(item transport.Item) *transport.Result { return ErrorReply(item, "INTERNAL", "same every time") })
	item := transport.Item{ID: "1", Method: "cash_status"}
	if r := h.answer(item, "RESTRICTED", false); r.Error.Code != "RESTRICTED" {
		t.Errorf("AnswerCode answered %+v", r.Error)
	}
	a, b := h.answer(item, "", true), h.answer(item, "", true)
	if a.Error.Code != "INTERNAL" || a.Error.Message == "same every time" || a.Error.Message == b.Error.Message {
		t.Errorf("random messages: %+v then %+v; want the code kept and a new message each time", a.Error, b.Error)
	}
	if r := h.answer(item, "", false); r.Error.Message != "same every time" {
		t.Errorf("message %q changed without RandomMessages", r.Error.Message)
	}
}
