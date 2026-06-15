package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// orgSpaceUserRow is one row from a users CSV used by create-org-space-users and
// delete-org-space-users.
//
// Fields may be empty to enable broadcast semantics:
//   - Empty Region   → target all active regions in credentials.
//   - Empty OrgID    → discover and target all accessible CF orgs.
//   - Empty SpaceID  → if SpaceName is set, target spaces with that name; if both
//     SpaceID and SpaceName are empty and the Roles contain space_* entries, target
//     all spaces in the resolved org.
//   - Non-empty UserID → skip FindCfUser lookup in delete operations (use GUID directly).
type orgSpaceUserRow struct {
	Region    string   // e.g. "eu20"; empty → broadcast across all active regions
	OrgID     string   // CF org GUID; empty → discover and target all accessible orgs
	OrgName   string
	SpaceID   string   // empty → org-level (or broadcast to spaces if space roles present)
	SpaceName string   // non-empty with empty SpaceID → match spaces by this name
	UserID    string   // cfuser_id (GUID); non-empty → skip FindCfUser in delete
	UserName  string   // cfuser_name
	Origin    string   // cfuser_origin
	Roles     []string // cfuser_roles, semicolon-separated
}

// parseOrgSpaceUsersCSV reads a users CSV file and returns a slice of orgSpaceUserRow.
//
// Two header formats are accepted:
//
// Simple 3-column format (broadcast — no targeting info):
//
//	cfuser_name,cfuser_origin,cfuser_roles
//
// Full 9-column format (produced by "bo org-space-users --format csv"):
//
//	region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles
//
// In both formats any non-user column may be empty.  An empty region, org_id, or
// space_id activates broadcast semantics — see create-org-space-users and
// delete-org-space-users for details.
func parseOrgSpaceUsersCSV(path string) ([]orgSpaceUserRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header: %w", err)
	}

	const (
		fullHeaderStr   = "region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles"
		simpleHeaderStr = "cfuser_name,cfuser_origin,cfuser_roles"
	)
	fullCols   := strings.Split(fullHeaderStr, ",")
	simpleCols := strings.Split(simpleHeaderStr, ",")

	var format string // "full" or "simple"
	switch {
	case len(header) >= len(fullCols) && csvHeaderMatches(header, fullCols):
		format = "full"
	case len(header) >= len(simpleCols) && csvHeaderMatches(header, simpleCols):
		format = "simple"
	default:
		return nil, fmt.Errorf("invalid --users CSV header — expected one of:\n  %s\n  %s", simpleHeaderStr, fullHeaderStr)
	}

	var rows []orgSpaceUserRow
	for line := 2; ; line++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		var osr orgSpaceUserRow
		switch format {
		case "simple":
			if len(row) < 3 {
				return nil, fmt.Errorf("line %d: expected 3 columns, got %d", line, len(row))
			}
			osr.UserName = strings.TrimSpace(row[0])
			osr.Origin = strings.TrimSpace(row[1])
			for _, v := range strings.Split(row[2], ";") {
				if v = strings.TrimSpace(v); v != "" {
					osr.Roles = append(osr.Roles, v)
				}
			}
		case "full":
			if len(row) < len(fullCols) {
				return nil, fmt.Errorf("line %d: expected %d columns, got %d", line, len(fullCols), len(row))
			}
			osr.Region = strings.TrimSpace(row[0])
			osr.OrgID = strings.TrimSpace(row[1])
			osr.OrgName = strings.TrimSpace(row[2])
			osr.SpaceID = strings.TrimSpace(row[3])
			osr.SpaceName = strings.TrimSpace(row[4])
			osr.UserID = strings.TrimSpace(row[5])
			osr.UserName = strings.TrimSpace(row[6])
			osr.Origin = strings.TrimSpace(row[7])
			for _, v := range strings.Split(row[8], ";") {
				if v = strings.TrimSpace(v); v != "" {
					osr.Roles = append(osr.Roles, v)
				}
			}
		}

		if osr.UserName == "" || osr.Origin == "" {
			return nil, fmt.Errorf("line %d: cfuser_name and cfuser_origin must not be empty", line)
		}
		rows = append(rows, osr)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV file contains no data rows")
	}
	return rows, nil
}

// csvHeaderMatches reports whether header[:len(want)] equals want (case-sensitive).
func csvHeaderMatches(header, want []string) bool {
	for i, w := range want {
		if header[i] != w {
			return false
		}
	}
	return true
}
