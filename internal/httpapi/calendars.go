package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"solo-0014-zone-rollout-control/internal/domain"
	"solo-0014-zone-rollout-control/internal/schedule"
)

func (s *Server) createCalendar(w http.ResponseWriter, r *http.Request) {
	var input domain.CalendarInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	calendar, err := s.calendars.Create(r.Context(), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, calendar)
}

func (s *Server) listCalendars(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			sendError(w, r, domain.Validation("limit must be an integer", map[string]any{"limit": raw}))
			return
		}
		limit = parsed
	}
	includeArchived := strings.TrimSpace(r.URL.Query().Get("include_archived")) == "true"
	page, err := s.calendars.List(
		r.Context(),
		includeArchived,
		limit,
		r.URL.Query().Get("cursor"),
	)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, page)
}

func (s *Server) getCalendar(w http.ResponseWriter, r *http.Request) {
	detail, err := s.calendars.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, detail)
}

func (s *Server) archiveCalendar(w http.ResponseWriter, r *http.Request) {
	if err := s.calendars.Archive(r.Context(), r.PathValue("id")); err != nil {
		sendError(w, r, err)
		return
	}
	detail, err := s.calendars.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, detail)
}

func (s *Server) addWindow(w http.ResponseWriter, r *http.Request) {
	var input domain.WindowInput
	if err := decode(w, r, &input); err != nil {
		sendError(w, r, err)
		return
	}
	entry, err := s.calendars.AddWindow(r.Context(), r.PathValue("id"), input)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusCreated, entry)
}

func (s *Server) archiveWindow(w http.ResponseWriter, r *http.Request) {
	if err := s.calendars.ArchiveWindow(
		r.Context(),
		r.PathValue("id"),
		r.PathValue("window_id"),
	); err != nil {
		sendError(w, r, err)
		return
	}
	detail, err := s.calendars.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, detail)
}

type admissionQueryInput struct {
	Zone   string     `json:"zone"`
	Region string     `json:"region"`
	At     *time.Time `json:"at"`
}

// resolveAdmission exposes the shared time interpretation without changing
// state. GET reads zone/region/at from the query string; PATCH accepts the
// same fields as JSON, including an RFC3339 "at" for hypothetical probes.
func (s *Server) resolveAdmission(w http.ResponseWriter, r *http.Request) {
	var input admissionQueryInput
	if r.Method == http.MethodPatch {
		if err := decode(w, r, &input); err != nil {
			sendError(w, r, err)
			return
		}
	} else {
		input.Zone = r.URL.Query().Get("zone")
		input.Region = r.URL.Query().Get("region")
		if raw := strings.TrimSpace(r.URL.Query().Get("at")); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				sendError(w, r, domain.Validation("at must be an RFC3339 timestamp", map[string]any{"at": raw}))
				return
			}
			input.At = &parsed
		}
	}
	if strings.TrimSpace(input.Zone) == "" {
		sendError(w, r, domain.Validation("zone is required", nil))
		return
	}
	var at time.Time
	if input.At != nil {
		at = input.At.UTC()
	}
	decision, err := s.calendars.Resolve(r.Context(), schedule.Scope{
		Zone:   input.Zone,
		Region: input.Region,
	}, at)
	if err != nil {
		sendError(w, r, err)
		return
	}
	sendData(w, r, http.StatusOK, decision)
}
