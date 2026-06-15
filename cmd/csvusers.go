package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// orgSpaceUserRow is one row from the org-space-users CSV output (produced by
// bo org-space-users --format csv). It is the --users input format for both
// create-org-space-users and delete-org-space-users, enabling precise per-org
// and per-space targeting without scanning all accessible CF resources.
//
// Rows with an empty SpaceID target the org level; rows with a non-empty
// SpaceID target that specific space.
type orgSpaceUserRow struct {
	Region    string   // e.g. "eu20"
	OrgID     string   // CF org GUID
	OrgName   string
	SpaceID   string   // empty → org-level; non-empty → space-level
	SpaceName string
	UserName  string   // cfuser_name
	Origin    string   // cfuser_origin
	Roles     []string // cfuser_roles, semicolon-separated
}

// parseOrgSpaceUsersCSV reads a CSV produced by "bo org-space-users --format csv".
//
// Required header (9 columns):
//
//	region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles
//
// Rows where space_id is empty are org-level; rows where space_id is non-empty
// are space-level. The cfuser_id column is informational and is not used by
// create-org-space-users or delete-org-space-users.
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
	const wantHeader = "region,org_id,org_name,space_id,space_name,cfuser_id,cfuser_name,cfuser_origin,cfuser_roles"
	wantCols := strings.Split(wantHeader, ",")
	if len(header) < len(wantCols) {
		return nil, fmt.Errorf("invalid header — expected: %s", wantHeader)
	}
	for i, want := range wantCols {
		if header[i] != want {
			return nil, fmt.Errorf("header column %d: expected %q, got %q", i+1, want, header[i])
		}
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
		if len(row) < len(wantCols) {
			return nil, fmt.Errorf("line %d: expected %d columns, got %d", line, len(wantCols), len(row))
		}
		region := strings.TrimSpace(row[0])
		orgID := strings.TrimSpace(row[1])
		orgName := strings.TrimSpace(row[2])
		spaceID := strings.TrimSpace(row[3])
		spaceName := strings.TrimSpace(row[4])
		// row[5] = cfuser_id — informational only; not used here
		userName := strings.TrimSpace(row[6])
		origin := strings.TrimSpace(row[7])
		if region == "" || orgID == "" || userName == "" || origin == "" {
			return nil, fmt.Errorf("line %d: region, org_id, cfuser_name, and cfuser_origin must not be empty", line)
		}
		var roles []string
		for _, v := range strings.Split(row[8], ";") {
			if v = strings.TrimSpace(v); v != "" {
				roles = append(roles, v)
			}
		}
		rows = append(rows, orgSpaceUserRow{
			Region:    region,
			OrgID:     orgID,
			OrgName:   orgName,
			SpaceID:   spaceID,
			SpaceName: spaceName,
			UserName:  userName,
			Origin:    origin,
			Roles:     roles,
		})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("CSV file contains no data rows")
	}
	return rows, nil
}
