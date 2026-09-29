package httpapi

import "net/http"

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.health)

	mux.HandleFunc("PUT /v1/nodes", s.enrollNode)
	mux.HandleFunc("GET /v1/nodes", s.listNodes)

	mux.HandleFunc("PUT /v1/plans", s.createPlan)
	mux.HandleFunc("GET /v1/plans", s.listPlans)
	mux.HandleFunc("GET /v1/plans/{id}", s.getPlan)
	mux.HandleFunc("PATCH /v1/plans/{id}/revisions", s.addRevision)
	mux.HandleFunc("PATCH /v1/plans/{id}/activate", s.activatePlan)
	mux.HandleFunc("PATCH /v1/plans/{id}/complete", s.completePlan)

	mux.HandleFunc("PATCH /v1/plans/{id}/leases", s.allocateLeases)
	mux.HandleFunc("PATCH /v1/leases/{id}/release", s.releaseLease)
	mux.HandleFunc("PATCH /v1/leases/{id}/probes", s.submitProbe)

	mux.HandleFunc("PATCH /v1/plans/{id}/assessments", s.assessPlan)
	mux.HandleFunc("GET /v1/plans/{id}/assessments/latest", s.latestAssessment)

	mux.HandleFunc("PUT /v1/calendars", s.createCalendar)
	mux.HandleFunc("GET /v1/calendars", s.listCalendars)
	mux.HandleFunc("GET /v1/calendars/{id}", s.getCalendar)
	mux.HandleFunc("PATCH /v1/calendars/{id}/archive", s.archiveCalendar)
	mux.HandleFunc("DELETE /v1/calendars/{id}", s.archiveCalendar)
	mux.HandleFunc("PUT /v1/calendars/{id}/windows", s.addWindow)
	mux.HandleFunc("DELETE /v1/calendars/{id}/windows/{window_id}", s.archiveWindow)
	mux.HandleFunc("GET /v1/calendar-admission", s.resolveAdmission)
	mux.HandleFunc("PATCH /v1/calendar-admission", s.resolveAdmission)
}
