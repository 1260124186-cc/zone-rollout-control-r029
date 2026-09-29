package httpapi

import (
	"net/http"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
)

func (s *Server) registerWindow(w http.ResponseWriter, r *http.Request) {
	var input domain.WindowInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	entry, err := s.calendars.Register(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, entry)
}

func (s *Server) replaceWindow(w http.ResponseWriter, r *http.Request) {
	var input domain.WindowInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	entry, err := s.calendars.Replace(
		r.Context(), r.PathValue("id"), r.PathValue("window_id"), input,
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, entry)
}

func (s *Server) deleteWindow(w http.ResponseWriter, r *http.Request) {
	if err := s.calendars.Delete(
		r.Context(), r.PathValue("id"), r.PathValue("window_id"),
	); err != nil {
		sendError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getWindow(w http.ResponseWriter, r *http.Request) {
	entry, err := s.calendars.Get(
		r.Context(), r.PathValue("id"), r.PathValue("window_id"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, entry)
}

func (s *Server) listCalendar(w http.ResponseWriter, r *http.Request) {
	entries, err := s.calendars.ListForPlan(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, map[string]any{"windows": entries})
}

func (s *Server) evaluateCalendar(w http.ResponseWriter, r *http.Request) {
	region := strings.TrimSpace(r.URL.Query().Get("region"))
	var at time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("at")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			sendError(w, r, domain.Validation("at must be an RFC3339 timestamp", map[string]any{
				"at": raw,
			}))
			return
		}
		at = parsed
	}
	decision, err := s.calendars.Evaluate(r.Context(), r.PathValue("id"), region, at)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, decision)
}
