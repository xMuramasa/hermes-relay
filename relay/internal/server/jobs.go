package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"

	"github.com/xMuramasa/trismegistus/relay/internal/hermes"
	"github.com/xMuramasa/trismegistus/relay/internal/protocol"
	"github.com/xMuramasa/trismegistus/relay/internal/store"
)

// Cron jobs: Hermes' /api/jobs behind the relay. Every device reads; only managers write, and writes carry only the
// fields Hermes' jobs API accepts (gateway/platforms/api_server.py, checked 2026-11-01). A job's model, provider,
// toolsets, script, workdir and context_from are shown but never written from the app.

const maxJobRuns = 50

var jobIDRe = regexp.MustCompile(`^[a-f0-9]{12}$`)

// jobFields are the job fields the app gets (cron/jobs.py names); the rest of Hermes' record (origin, monitor
// settings, delivery bookkeeping) stays behind.
var jobFields = []string{"id", "name", "prompt", "schedule", "schedule_display", "deliver", "failure_deliver",
	"skills", "repeat", "enabled", "state", "paused_at", "paused_reason", "model", "provider", "enabled_toolsets",
	"script", "workdir", "context_from", "created_at", "next_run_at", "last_run_at", "last_status", "last_error"}

// jobCreate is POST /api/jobs: Hermes' create fields, nothing else.
type jobCreate struct {
	Name         *string   `json:"name,omitempty"`
	Schedule     *string   `json:"schedule,omitempty"`
	Prompt       *string   `json:"prompt,omitempty"`
	Deliver      *string   `json:"deliver,omitempty"`
	Skills       *[]string `json:"skills,omitempty"`
	Repeat       *int      `json:"repeat,omitempty"`
	Paused       *bool     `json:"paused,omitempty"`
	PausedReason *string   `json:"paused_reason,omitempty"`
}

// jobUpdate is PATCH /api/jobs/{id}: Hermes' update fields, nothing else.
type jobUpdate struct {
	Name     *string   `json:"name,omitempty"`
	Schedule *string   `json:"schedule,omitempty"`
	Prompt   *string   `json:"prompt,omitempty"`
	Deliver  *string   `json:"deliver,omitempty"`
	Skills   *[]string `json:"skills,omitempty"`
	Skill    *string   `json:"skill,omitempty"`
	Repeat   *int      `json:"repeat,omitempty"`
	Enabled  *bool     `json:"enabled,omitempty"`
}

// jobID reads and checks the {id} path segment (Hermes' job id shape).
func jobID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !jobIDRe.MatchString(id) {
		writeError(w, protocol.Errorf(protocol.CodeNotFound, "not found"), 0)
		return "", false
	}
	return id, true
}

func trimJob(job map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(jobFields))
	for _, k := range jobFields {
		if v, ok := job[k]; ok {
			out[k] = v
		}
	}
	return out
}

// jobError maps a failed jobs call, keeping Hermes' message where it explains the refusal.
func jobError(resp *hermes.Response, err error) *protocol.Error {
	msg := ""
	if resp != nil {
		msg = hermes.ErrorMessage(resp.Body)
	}
	switch hermes.StatusOf(err) {
	case http.StatusBadRequest:
		if msg != "" {
			return protocol.Errorf(protocol.CodeBadRequest, "%s", msg)
		}
	case http.StatusInternalServerError:
		// Hermes answers 500 for refusals raised inside the scheduler too (bad schedule on update, terminal job).
		if msg != "" {
			return protocol.Errorf(protocol.CodeHermesError, "%s", msg)
		}
	case http.StatusNotImplemented:
		return protocol.Errorf(protocol.CodeHermesUnavailable, "Hermes' cron scheduler is not available")
	case http.StatusFailedDependency:
		return protocol.Errorf(protocol.CodeHermesError, "the job was saved but its schedule was not registered: %s", msg)
	}
	return hermes.ProtocolError(err)
}

// forwardJobs sends one jobs request and answers {"jobs": [...]} or {"job": {...}} (key) trimmed to jobFields, or
// {"ok": true} when key is "". A non-empty action (a write) is logged with the job id and outcome, never the body.
func (s *Server) forwardJobs(w http.ResponseWriter, r *http.Request, dev store.Device, action, id, method, path string,
	q url.Values, body []byte, key string) {
	outcome := "ok"
	if action != "" {
		defer func() { s.log.Info("job "+action, "job_id", id, "device", dev.Name, "outcome", outcome) }()
	}
	resp, err := s.hermes.Do(r.Context(), method, path, q, body, nil)
	if err != nil {
		if st := hermes.StatusOf(err); st != 0 {
			s.log.Warn("hermes refused", "hermes_status", st)
		}
		e := jobError(resp, err)
		outcome = e.Code
		writeError(w, e, hermes.RetryAfterOf(err))
		return
	}
	var in struct {
		Jobs []map[string]json.RawMessage `json:"jobs"`
		Job  map[string]json.RawMessage   `json:"job"`
	}
	out := map[string]any{}
	bad := json.Unmarshal(resp.Body, &in) != nil
	switch key {
	case "jobs":
		bad = bad || in.Jobs == nil
		jobs := make([]map[string]json.RawMessage, 0, len(in.Jobs))
		for _, j := range in.Jobs {
			jobs = append(jobs, trimJob(j))
		}
		out[key] = jobs
	case "job":
		bad = bad || in.Job == nil
		out[key] = trimJob(in.Job)
		if id == "" {
			_ = json.Unmarshal(in.Job["id"], &id)
		}
	default:
		out["ok"] = true
	}
	if bad {
		outcome = protocol.CodeHermesError
		writeError(w, protocol.Errorf(protocol.CodeHermesError, "Hermes sent an invalid response"), 0)
		return
	}
	writeJSON(w, resp.Status, out)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, dev store.Device) {
	// Paused jobs are disabled in Hermes, which leaves them out unless asked.
	s.forwardJobs(w, r, dev, "", "", http.MethodGet, "/api/jobs", url.Values{"include_disabled": {"true"}}, nil, "jobs")
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, dev store.Device) {
	if id, ok := jobID(w, r); ok {
		s.forwardJobs(w, r, dev, "", id, http.MethodGet, "/api/jobs/"+id, nil, nil, "job")
	}
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request, dev store.Device) {
	var body jobCreate
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	b, _ := json.Marshal(body)
	s.forwardJobs(w, r, dev, "create", "", http.MethodPost, "/api/jobs", nil, b, "job")
}

func (s *Server) updateJob(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	var body jobUpdate
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, err, 0)
		return
	}
	b, _ := json.Marshal(body)
	s.forwardJobs(w, r, dev, "update", id, http.MethodPatch, "/api/jobs/"+id, nil, b, "job")
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request, dev store.Device) {
	if id, ok := jobID(w, r); ok {
		s.forwardJobs(w, r, dev, "delete", id, http.MethodDelete, "/api/jobs/"+id, nil, nil, "")
	}
}

// jobAction is POST /api/jobs/{id}/pause|resume|run. Any body is ignored: run's one-off extra prompt isn't offered.
func (s *Server) jobAction(action string) func(http.ResponseWriter, *http.Request, store.Device) {
	return func(w http.ResponseWriter, r *http.Request, dev store.Device) {
		if id, ok := jobID(w, r); ok {
			s.forwardJobs(w, r, dev, action, id, http.MethodPost, "/api/jobs/"+id+"/"+action, nil, nil, "job")
		}
	}
}

// jobRuns lists the sessions of a job's runs, newest first, so the app can open one.
func (s *Server) jobRuns(w http.ResponseWriter, r *http.Request, _ store.Device) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	runs, err := s.hermes.CronRuns(r.Context(), id, maxJobRuns)
	if err != nil {
		s.hermesFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}
