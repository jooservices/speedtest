package server

import "net/http"

func parseEngine(w http.ResponseWriter, r *http.Request) (string, bool) {
	values, present := r.URL.Query()["engine"]
	if !present {
		return "", true
	}
	if len(values) != 1 || (values[0] != "ookla" && values[0] != "librespeed") {
		return "", invalid(w, r, "engine must be ookla or librespeed.")
	}
	return values[0], true
}
