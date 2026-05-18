package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"
)

// csvUser represents one row from a users CSV file (name,origin,roles).
type csvUser struct {
	Name   string
	Origin string
	Roles  []string
}

// parseUsersCSV reads a CSV file with header "name,origin,roles" and returns
// the list of users. Roles within a row are semicolon-separated.
func parseUsersCSV(path string) ([]csvUser, error) {
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
	if len(header) < 3 || header[0] != "name" || header[1] != "origin" || header[2] != "roles" {
		return nil, fmt.Errorf("invalid header — expected: name,origin,roles")
	}

	var users []csvUser
	for line := 2; ; line++ {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if len(row) < 3 {
			return nil, fmt.Errorf("line %d: expected 3 columns, got %d", line, len(row))
		}
		name := strings.TrimSpace(row[0])
		origin := strings.TrimSpace(row[1])
		if name == "" || origin == "" {
			return nil, fmt.Errorf("line %d: name and origin cannot be empty", line)
		}
		var roles []string
		for _, r := range strings.Split(row[2], ";") {
			if v := strings.TrimSpace(r); v != "" {
				roles = append(roles, v)
			}
		}
		users = append(users, csvUser{Name: name, Origin: origin, Roles: roles})
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("CSV file contains no user rows")
	}
	return users, nil
}
