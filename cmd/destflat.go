package cmd

import (
	"fmt"
	"sort"
	"strings"
)

// This file provides --fields/--sort-by support for the destination-family
// commands (subaccount-destinations, space-destinations, destinations),
// whose rows can't use fieldsort.go's generic, fixed-struct-based helpers:
// a destination's own properties (Name, URL, Authentication, ProxyType,
// sap-client, ...) vary per destination and aren't known at compile time.
// destFlatRow instead separates each row's fixed structural identity
// (org/space/instance — a command uses only the columns that apply to it,
// leaving the rest empty) from its dynamic Props map.

// destFlatRow is one destination row: structural identity columns (not all
// commands use every one — subaccount-destinations only ever sets
// OrgID/OrgName, leaving the rest empty) plus the destination's own raw
// properties, exactly as the API returned them (case-sensitive keys).
type destFlatRow struct {
	OrgID        string
	OrgName      string
	SpaceID      string
	SpaceName    string
	InstanceID   string
	InstanceName string
	Props        map[string]string
}

// destFlatRowValue returns row's value for a column name: the structural
// fields first (org_id, org_name, space_id, space_name, instance_id,
// instance_name), then its own properties — an exact, case-sensitive match,
// since destination property names have inconsistent casing from the API
// itself (e.g. "Name" vs "sap-client").
func destFlatRowValue(row destFlatRow, key string) string {
	switch key {
	case "org_id":
		return row.OrgID
	case "org_name":
		return row.OrgName
	case "space_id":
		return row.SpaceID
	case "space_name":
		return row.SpaceName
	case "instance_id":
		return row.InstanceID
	case "instance_name":
		return row.InstanceName
	default:
		return row.Props[key]
	}
}

// destParseFields splits --fields into an ordered, trimmed list of column
// names, with no validation against a fixed list — destination property
// names are inherently open-ended, so any name the caller gives is used
// as-is (a name matching nothing just renders as an empty column).
func destParseFields(raw string) []string {
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, f := range splitCSV(raw) {
		out = append(out, strings.TrimSpace(f))
	}
	return out
}

// destSortKey is one --sort-by criterion: a column name plus direction.
type destSortKey struct {
	Key  string
	Desc bool
}

// destParseSortBy parses --sort-by the same way parseSortByFlag does
// (comma-separated keys, each optionally suffixed with ":desc"/":asc",
// preserved as tie-breaking priority order), but without validating key
// names against a fixed list, for the same reason as destParseFields.
func destParseSortBy(raw, flagName string) ([]destSortKey, error) {
	if raw == "" {
		return nil, nil
	}
	keys := make([]destSortKey, 0, 4)
	for _, f := range splitCSV(raw) {
		f = strings.TrimSpace(f)
		key, dir := f, ""
		if idx := strings.LastIndex(f, ":"); idx >= 0 {
			key = strings.TrimSpace(f[:idx])
			dir = strings.ToLower(strings.TrimSpace(f[idx+1:]))
		}
		var desc bool
		switch dir {
		case "", "asc":
			desc = false
		case "desc":
			desc = true
		default:
			return nil, fmt.Errorf("invalid %s direction %q for field %q: must be \"asc\" or \"desc\"", flagName, dir, key)
		}
		keys = append(keys, destSortKey{Key: key, Desc: desc})
	}
	return keys, nil
}

// destSortRowsBy sorts rows in place by keys, in priority order (ties on
// one key are broken by the next), stably preserving the existing order
// for anything keys doesn't distinguish.
func destSortRowsBy(rows []destFlatRow, keys []destSortKey) {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			vi, vj := destFlatRowValue(rows[i], k.Key), destFlatRowValue(rows[j], k.Key)
			if vi == vj {
				continue
			}
			if k.Desc {
				return vi > vj
			}
			return vi < vj
		}
		return false
	})
}

// destColumnOrder returns fieldIDs if given, else leadingCols (a command's
// own structural columns, in its own fixed order) followed by the union of
// every row's Props keys actually present in this result set, sorted
// alphabetically for a deterministic header.
func destColumnOrder(rows []destFlatRow, fieldIDs, leadingCols []string) []string {
	if len(fieldIDs) > 0 {
		return fieldIDs
	}
	propSet := make(map[string]bool)
	for _, r := range rows {
		for k := range r.Props {
			propSet[k] = true
		}
	}
	propList := make([]string, 0, len(propSet))
	for k := range propSet {
		propList = append(propList, k)
	}
	sort.Strings(propList)
	cols := make([]string, 0, len(leadingCols)+len(propList))
	cols = append(cols, leadingCols...)
	cols = append(cols, propList...)
	return cols
}

// destWriteCSV writes rows as CSV, with destColumnOrder(rows, fieldIDs,
// leadingCols) as the header and column order.
func destWriteCSV(csvW csvWriter, rows []destFlatRow, fieldIDs, leadingCols []string) error {
	cols := destColumnOrder(rows, fieldIDs, leadingCols)
	if err := csvW.Write(cols); err != nil {
		return err
	}
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = destFlatRowValue(r, c)
		}
		if err := csvW.Write(cells); err != nil {
			return err
		}
	}
	return nil
}

// destRowMap projects row down to a plain map for JSON/TOON output: exactly
// fieldIDs if given (missing values render as "", matching destWriteCSV),
// else leadingCols merged with the row's own Props (so a command with no
// space/instance concept doesn't emit empty space_id/instance_id keys).
func destRowMap(row destFlatRow, fieldIDs, leadingCols []string) map[string]string {
	if len(fieldIDs) > 0 {
		m := make(map[string]string, len(fieldIDs))
		for _, f := range fieldIDs {
			m[f] = destFlatRowValue(row, f)
		}
		return m
	}
	m := make(map[string]string, len(leadingCols)+len(row.Props))
	for _, c := range leadingCols {
		m[c] = destFlatRowValue(row, c)
	}
	for k, v := range row.Props {
		m[k] = v
	}
	return m
}

// destRowMaps applies destRowMap across rows, for JSON/TOON output.
func destRowMaps(rows []destFlatRow, fieldIDs, leadingCols []string) []map[string]string {
	out := make([]map[string]string, len(rows))
	for i, r := range rows {
		out[i] = destRowMap(r, fieldIDs, leadingCols)
	}
	return out
}
