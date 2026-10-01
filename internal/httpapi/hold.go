package httpapi

import (
	"net/http"

	"example.com/paleo-prep-station/internal/contracts"
)

func (api *API) holdPlan(w http.ResponseWriter, r *http.Request) {
	var request contracts.HoldRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	value, err := api.service.HoldPlan(r.Context(), r.PathValue("id"), request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (api *API) resumePlan(w http.ResponseWriter, r *http.Request) {
	var request contracts.ResumeRequest
	if !decodeJSON(w, r, &request) {
		return
	}
	value, err := api.service.ResumePlan(r.Context(), r.PathValue("id"), request)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
