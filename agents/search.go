package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// searchScript runs inside Hermes' interpreter; see search.py.
//
//go:embed search.py
var searchScript string

// maxSearches run at once; more wait for a slot.
const maxSearches = 2

// found is search.py's answer, passed on to the relay.
type found struct {
	Results []json.RawMessage `json:"results"`
	More    bool              `json:"more"`
}

// serveSearch answers GET /search?q=…&limit=n with Hermes' message index (SessionDB.search_messages), re-checked
// against each message's own text by search.py. Neither the query nor a snippet is ever logged.
func (h *Helper) serveSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := utf8.RuneCountInString(q); n < 2 || n > 200 {
		writeErr(w, http.StatusBadRequest, "bad_request", "q must be 2 to 200 characters")
		return
	}
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		var err error
		if limit, err = strconv.Atoi(s); err != nil || limit < 1 || limit > 100 {
			writeErr(w, http.StatusBadRequest, "bad_request", "limit must be 1 to 100")
			return
		}
	}
	select {
	case h.searches <- struct{}{}:
		defer func() { <-h.searches }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.SearchTimeout)
	defer cancel()
	line, _, err := h.python(ctx, searchScript, q, strconv.Itoa(limit))
	var out found
	if err == nil && (json.Unmarshal([]byte(line), &out) != nil || out.Results == nil) {
		err = errNoSearchResult
	}
	if err != nil {
		h.Log.Warn("search failed") // the reason may quote the query
		writeErr(w, http.StatusBadGateway, "search_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var errNoSearchResult = errors.New("the search script printed no result")
