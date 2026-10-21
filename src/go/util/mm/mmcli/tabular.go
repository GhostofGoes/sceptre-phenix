// Taken (almost) as-is from minimega/miniweb.

package mmcli

import (
	"strings"

	"github.com/activeshadow/libminimega/minicli"
	"github.com/hashicorp/go-multierror"

	"phenix/util/plog"
)

type tabularToMapper func(*minicli.Response, []string) map[string]string

func tabularToMap(resp *minicli.Response, row []string) map[string]string {
	res := map[string]string{
		"host": resp.Host,
	}

	for i, header := range resp.Header {
		res[header] = row[i]
	}

	return res
}

func tabularToMapCols(columns []string) tabularToMapper {
	// create local copy of columns in case they get changed
	cols := make([]string, len(columns))
	copy(cols, columns)

	return func(resp *minicli.Response, row []string) map[string]string {
		res := map[string]string{}

		for _, column := range cols {
			if strings.Contains(column, "host") {
				res["host"] = resp.Host

				continue
			}

			for i, header := range resp.Header {
				if strings.Contains(column, header) {
					res[header] = row[i]
				}
			}
		}

		return res
	}
}

// RunTabular is used to run the given command when the response is expected to
// be in tabular form. A slice of maps is returned, with each map representing a
// row in the tabular response and each map key representing the column. Errors
// are logged and otherwise dropped, so a failed command reads as an empty
// table; use RunTabularErr when the caller must tell the two apart.
func RunTabular(cmd *Command) []map[string]string {
	return runTabular(cmd, func(msg string) {
		plog.Error(
			plog.TypeSystem,
			"error running mm cmd",
			"cmd",
			cmd.Command,
			"error",
			msg,
		)
	})
}

// RunTabularErr is RunTabular for callers that must tell a failure apart from
// an empty table. It returns the rows of every response that succeeded along
// with a *multierror.Error holding every error minimega, or the connection to
// it, reported. Both can be set at once: minimega answers a namespaced command
// once per host, and some hosts may fail where others succeed.
func RunTabularErr(cmd *Command) ([]map[string]string, error) {
	var errs error

	rows := runTabular(cmd, func(msg string) {
		errs = multierror.Append(errs, reconstructErr(msg))
	})

	return rows, errs
}

// runTabular runs cmd and collects the rows of its tabular responses, handing
// the text of each error response to onErr.
func runTabular(cmd *Command, onErr func(string)) []map[string]string {
	// copy all fields in header order
	mapper := tabularToMap

	if len(cmd.Columns) > 0 {
		// replace mapper to only copy fields in column order
		mapper = tabularToMapCols(cmd.Columns)
	}

	res := []map[string]string{}

	for resps := range Run(cmd) {
		for _, resp := range resps.Resp {
			if resp.Error != "" {
				onErr(resp.Error)

				continue
			}

			for _, row := range resp.Tabular {
				res = append(res, mapper(resp, row))
			}
		}
	}

	return res
}
