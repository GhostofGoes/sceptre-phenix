package web

import (
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"phenix/util/mm"
)

// refuseUnsafeVMNames answers 400 to a request for a VM route, one naming an
// experiment {exp} and a VM {name}, whose VM name minimega could not take as
// one argument of a namespaced command, or would read as more than that one
// VM. The VM handlers put the name into minimega commands as given: minimega
// exits when a namespaced command it rebuilds for its hosts fails to compile,
// and it reads a name such as web1,db1 or all as a list of VMs, which RBAC
// would have authorized only as the literal name.
func refuseUnsafeVMNames(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			vars      = mux.Vars(r)
			_, hasExp = vars["exp"]
			name, ok  = vars["name"]
		)

		if hasExp && ok {
			if err := mm.CheckVMName(name); err != nil {
				http.Error(w, fmt.Sprintf("invalid VM name: %v", err), http.StatusBadRequest)

				return
			}
		}

		next.ServeHTTP(w, r)
	})
}
