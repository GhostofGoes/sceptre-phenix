package web

import (
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"phenix/web/cache"
)

// refuseStoppingExperiments answers 409 to a request for a route of an
// experiment {exp}, the VM, netflow and capture routes, while this process
// stops the experiment. Their handlers check that the experiment is running
// before they send minimega namespaced commands, and that check passes until
// the stop has cleared the namespace and then the experiment's start time, so
// a command in between would create the namespace again.
func refuseStoppingExperiments(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exp, ok := mux.Vars(r)["exp"]; ok && cache.IsExperimentStopping(exp) {
			http.Error(w, fmt.Sprintf("experiment %s is stopping", exp), http.StatusConflict)

			return
		}

		next.ServeHTTP(w, r)
	})
}
