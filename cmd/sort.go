package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// This file implements 'bo sort', a standalone file-editing utility: unlike
// every other command in this package, it never talks to BTP/CF at all — it
// just sorts the rows of CSV or JSON files already on disk (e.g. files a
// previous 'bo' command wrote with --output). Its row representation is
// necessarily generic (map[string]interface{}), since the schema of an
// arbitrary input file isn't known at compile time — the same reasoning
// behind cmd/destflat.go's dynamic row handling, applied here to whole files
// instead of one command's own output.

// sortByKey is one --by criterion: a field name plus direction.
type sortByKey struct {
	Field string
	Desc  bool
}

// parseSortByKeys parses --by's comma-separated field list, each optionally
// suffixed with ":desc"/":asc" (":asc" is the default), preserving the order
// given as sort priority. Field names are not validated against any fixed
// list — like --fields below, and for the same reason: a field absent from a
// particular file's data just compares as empty for that file, with no error
// (several heterogeneous files can be given to one invocation). Returns nil
// when raw is empty, meaning "no explicit sort — use every available field".
func parseSortByKeys(raw string) ([]sortByKey, error) {
	if raw == "" {
		return nil, nil
	}
	keys := make([]sortByKey, 0, 4)
	for _, f := range splitCSV(raw) {
		f = strings.TrimSpace(f)
		field, dir := f, ""
		if idx := strings.LastIndex(f, ":"); idx >= 0 {
			field = strings.TrimSpace(f[:idx])
			dir = strings.ToLower(strings.TrimSpace(f[idx+1:]))
		}
		if field == "" {
			return nil, fmt.Errorf("invalid --by value %q: empty field name", f)
		}
		var desc bool
		switch dir {
		case "", "asc":
			desc = false
		case "desc":
			desc = true
		default:
			return nil, fmt.Errorf("invalid --by direction %q for field %q: must be \"asc\" or \"desc\"", dir, field)
		}
		keys = append(keys, sortByKey{Field: field, Desc: desc})
	}
	return keys, nil
}

// parseSortFields splits --fields into an ordered, trimmed list of field
// names, with no validation (see parseSortByKeys).
func parseSortFields(raw string) []string {
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 4)
	for _, f := range splitCSV(raw) {
		out = append(out, strings.TrimSpace(f))
	}
	return out
}

// sortCompareValues orders two row values: numerically if both sides parse
// as a number (a JSON number decodes as json.Number; a CSV cell is always a
// string, parsed with strconv), otherwise as plain text. A missing value
// (map lookup miss) comes through as a nil interface{}, which compares as
// the empty string against anything that isn't also nil.
func sortCompareValues(a, b interface{}) int {
	if af, aok := sortValueAsFloat(a); aok {
		if bf, bok := sortValueAsFloat(b); bok {
			switch {
			case af < bf:
				return -1
			case af > bf:
				return 1
			default:
				return 0
			}
		}
	}
	if ab, aok := a.(bool); aok {
		if bb, bok := b.(bool); bok {
			switch {
			case ab == bb:
				return 0
			case !ab && bb:
				return -1
			default:
				return 1
			}
		}
	}
	as, bs := sortValueAsString(a), sortValueAsString(b)
	switch {
	case as < bs:
		return -1
	case as > bs:
		return 1
	default:
		return 0
	}
}

// sortValueAsFloat reports v's numeric value, if it has one: a JSON number
// (decoded as json.Number — see readSortJSON), or a CSV cell (a plain
// string) that parses as a number.
func sortValueAsFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		if strings.TrimSpace(x) == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// sortValueAsString renders v for a text comparison or a CSV cell.
func sortValueAsString(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return fmt.Sprintf("%t", x)
	default:
		// A nested JSON object/array landing here means --by named a
		// non-leaf field; compare by its JSON form rather than crashing.
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// resolveSortFieldValue returns row's value for field. A plain field name
// (no dot) is just row[field] — nil if absent, the existing lenient
// behavior for any unknown field. A dotted field name (e.g.
// "app_labels.commit_id") instead walks row[field]'s first segment, then
// each further dot-separated segment as a key into the previous segment's
// own value: a segment's value is decomposed into an object first if
// necessary — either already a map (a native JSON object) or a string that
// itself decodes as one (e.g. a CSV cell, or a JSON string field holding
// JSON, both exactly the "app_labels" shape 'bo apps --include-labels'
// writes). The moment some segment's value is neither of those, resolution
// stops there and that segment's own raw value is returned as-is instead
// of nil — "not an object or JSON string, so treat the whole thing as
// plain text" — which for the common one-dot case is exactly the named
// parent field's own whole value. A segment that properly decomposes but
// simply doesn't have the next key returns nil, same as any other missing
// field.
func resolveSortFieldValue(row map[string]interface{}, field string) interface{} {
	if !strings.Contains(field, ".") {
		return row[field]
	}
	segments := strings.Split(field, ".")
	cur, ok := row[segments[0]]
	if !ok {
		return nil
	}
	for _, seg := range segments[1:] {
		obj, isObj := asJSONObject(cur)
		if !isObj {
			return cur
		}
		cur = obj[seg]
	}
	return cur
}

// asJSONObject reports whether v is already a JSON object (decoded as
// map[string]interface{}) or a string that decodes as one, returning it
// either way as a map[string]interface{} — used by resolveSortFieldValue
// to walk into a dotted field one segment at a time.
func asJSONObject(v interface{}) (map[string]interface{}, bool) {
	switch x := v.(type) {
	case map[string]interface{}:
		return x, true
	case string:
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(x), &m); err != nil {
			return nil, false
		}
		return m, true
	default:
		return nil, false
	}
}

// sortHashFieldInner reports whether field is a "hash(...)" --fields entry
// (e.g. "hash(password)", or "hash(app_labels.commit_id)" — the inner name
// can be dotted too), returning the inner field name with ok=true if so.
// "hash()" (empty inner) and anything not of the exact prefix/suffix shape
// don't match (ok=false) and are left to be treated as an ordinary,
// literal field name instead — consistent with --fields/--by's existing
// "no validation" convention elsewhere in this file.
func sortHashFieldInner(field string) (inner string, ok bool) {
	const prefix, suffix = "hash(", ")"
	if !strings.HasPrefix(field, prefix) || !strings.HasSuffix(field, suffix) {
		return "", false
	}
	inner = strings.TrimSpace(field[len(prefix) : len(field)-len(suffix)])
	if inner == "" {
		return "", false
	}
	return inner, true
}

// sortHashValue renders v (see sortValueAsString) and returns its SHA-256
// hex digest — a one-way, irreversible fingerprint, not the value itself.
// Used by a "hash(...)" --fields entry so a sensitive field (a password,
// client secret, etc.) can be included in a sorted/compared result without
// ever exposing its actual value: two exports' hashes match exactly when
// the underlying value is exactly the same, and differ otherwise, which is
// everything a diff/comparison needs. An empty/absent value hashes to ""
// (not SHA-256's own hash-of-empty-string), so a field that's genuinely
// missing or empty doesn't render as if some value were present.
func sortHashValue(v interface{}) string {
	s := sortValueAsString(v)
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// resolveSortFieldOrHash is resolveSortFieldValue, except a "hash(...)"
// field (see sortHashFieldInner) resolves its inner field name instead and
// returns that value's SHA-256 hex digest (see sortHashValue) rather than
// the value itself. Used wherever a field's value is rendered or compared —
// CSV cells, a JSON row's additive synthetic fields, and sortRowsByKeys —
// so a --by (explicit, or the implicit default sort from --fields) naming
// a "hash(...)" entry sorts by its hex digest, grouping rows with identical
// underlying secrets together without ever comparing the real value.
func resolveSortFieldOrHash(row map[string]interface{}, field string) interface{} {
	if inner, ok := sortHashFieldInner(field); ok {
		return sortHashValue(resolveSortFieldValue(row, inner))
	}
	return resolveSortFieldValue(row, field)
}

// parseWholeJSON decodes s as a single JSON value, using json.Number so a
// number's exact textual form round-trips (e.g. "1.50" doesn't become
// "1.5"), and rejects it (ok=false) unless the entire string — besides
// trailing whitespace — was consumed: encoding/json's Decoder, unlike
// json.Unmarshal, stops after one value and silently ignores whatever
// follows, which would otherwise treat a non-JSON string that happens to
// start with valid JSON syntax (e.g. "[1,2]extra") as if it were clean JSON.
func parseWholeJSON(s string) (interface{}, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// normalizeNestedFieldValue returns val with any JSON array/object or plain
// comma-separated list content it holds sorted into a canonical order —
// used by --sort-nested-fields so two otherwise-equivalent exports that
// only differ in some list/object's internal ordering don't show up as a
// diff. It recurses into nested arrays/objects, so a list of objects (or an
// object holding a list) is normalized throughout, not just at the top
// level. A native JSON array/object (as decoded from a JSON input file) is
// handled directly; a string is first tried as JSON — covering both a CSV
// cell holding JSON-encoded content and a JSON field whose own value is a
// JSON-encoded string — and, failing that, treated as a plain
// comma-separated list (split, trimmed, sorted, rejoined with ","). A
// scalar, or a string that's neither of the above (not valid JSON and not a
// list), is returned unchanged — there's nothing to sort.
func normalizeNestedFieldValue(val interface{}) interface{} {
	switch v := val.(type) {
	case []interface{}:
		return sortJSONArrayValue(v)
	case map[string]interface{}:
		return sortJSONObjectValue(v)
	case string:
		return normalizeNestedFieldString(v)
	default:
		return val
	}
}

// sortJSONArrayValue normalizes each element of arr (recursively, so
// elements that are themselves objects/arrays are normalized throughout)
// and then sorts the (now-normalized) elements into a canonical order,
// using the same numeric-aware-else-text comparison as --by.
func sortJSONArrayValue(arr []interface{}) []interface{} {
	out := make([]interface{}, len(arr))
	for i, el := range arr {
		out[i] = normalizeNestedFieldValue(el)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return sortCompareValues(out[i], out[j]) < 0
	})
	return out
}

// sortJSONObjectValue normalizes every value in obj, recursively — an
// object's own keys need no sorting here since encoding/json already
// serializes map keys alphabetically whenever this is re-marshaled.
func sortJSONObjectValue(obj map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(obj))
	for k, v := range obj {
		out[k] = normalizeNestedFieldValue(v)
	}
	return out
}

// splitNestedListItems splits a plain (non-JSON) field value into its list
// items for --sort-nested-fields, trimming whitespace from each and
// dropping empty ones, and reports the delimiter actually used so the
// caller can rejoin with that same one. ',' is the delimiter whenever s
// contains one, exactly like splitCSV elsewhere; ';' is only used as a
// fallback when s contains no ',' at all but does contain ';' (e.g. a tags
// field written as "a;b;c" to sidestep a CSV column's own comma). Anything
// with neither delimiter is just one single item.
func splitNestedListItems(s string) (items []string, delim string) {
	delim = ","
	if !strings.Contains(s, ",") && strings.Contains(s, ";") {
		delim = ";"
	}
	for _, part := range strings.Split(s, delim) {
		if v := strings.TrimSpace(part); v != "" {
			items = append(items, v)
		}
	}
	return items, delim
}

// normalizeNestedFieldString is normalizeNestedFieldValue's string case: a
// string holding valid JSON (array/object) is parsed, normalized, and
// re-encoded back to the same JSON string shape; anything else is treated
// as a plain delimited list (see splitNestedListItems for the ','/';'
// delimiter rule) and sorted/rejoined with that same delimiter. A JSON
// scalar (number/bool/string/null), or an empty/blank string, is returned
// unchanged.
func normalizeNestedFieldString(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	if parsed, ok := parseWholeJSON(s); ok {
		switch parsed.(type) {
		case []interface{}, map[string]interface{}:
			if out, err := json.Marshal(normalizeNestedFieldValue(parsed)); err == nil {
				return string(out)
			}
		}
		return s
	}
	parts, delim := splitNestedListItems(s)
	sort.SliceStable(parts, func(i, j int) bool {
		return sortCompareValues(parts[i], parts[j]) < 0
	})
	return strings.Join(parts, delim)
}

// applySortNestedFields normalizes (see normalizeNestedFieldValue) each
// row's value for every plain, top-level field name in fields, mutating
// rows in place. A field absent from a given row is left alone — the same
// lenient, no-error semantics as --by/--fields.
func applySortNestedFields(rows []map[string]interface{}, fields []string) {
	if len(fields) == 0 {
		return
	}
	for _, row := range rows {
		for _, f := range fields {
			if v, ok := row[f]; ok {
				row[f] = normalizeNestedFieldValue(v)
			}
		}
	}
}

// sortRowsByKeys sorts rows in place by keys, in priority order (ties on one
// key are broken by the next), stably preserving the existing order for
// anything keys doesn't distinguish.
func sortRowsByKeys(rows []map[string]interface{}, keys []sortByKey) {
	sort.SliceStable(rows, func(i, j int) bool {
		for _, k := range keys {
			c := sortCompareValues(resolveSortFieldOrHash(rows[i], k.Field), resolveSortFieldOrHash(rows[j], k.Field))
			if c == 0 {
				continue
			}
			if k.Desc {
				return c > 0
			}
			return c < 0
		}
		return false
	})
}

// defaultSortKeys builds the implicit sort-key list used when --by is
// omitted: fields' own order, if --fields was given — once you've already
// narrowed down to specific columns, sorting by exactly those is normally
// what you want, and it's what keeps two separately-sorted files comparable
// (naturalOrder, below, often starts with columns --fields would have
// dropped anyway, e.g. a CSV's leading GUID columns) — otherwise
// naturalOrder (a CSV's header order, or a JSON file's alphabetical leaf
// field order; see readSortCSV/jsonLeafFields).
func defaultSortKeys(fields, naturalOrder []string) []sortByKey {
	order := naturalOrder
	if len(fields) > 0 {
		order = fields
	}
	keys := make([]sortByKey, len(order))
	for i, f := range order {
		keys[i] = sortByKey{Field: f}
	}
	return keys
}

// ── CSV ──────────────────────────────────────────────────────────────────────

// readSortCSV reads path's header row and data rows, returning each data row
// as a map keyed by header column name (every value a plain string). A
// short row is padded with empty strings for its missing trailing columns.
func readSortCSV(path string) (header []string, rows []map[string]interface{}, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CSV: %w", err)
	}
	if len(records) == 0 {
		return nil, nil, nil
	}
	header = records[0]
	rows = make([]map[string]interface{}, 0, len(records)-1)
	for _, rec := range records[1:] {
		row := make(map[string]interface{}, len(header))
		for i, h := range header {
			if i < len(rec) {
				row[h] = rec[i]
			} else {
				row[h] = ""
			}
		}
		rows = append(rows, row)
	}
	return header, rows, nil
}

// writeSortCSV writes header as the header row, then one row per rows,
// reading each column by name (a column --fields names that a row doesn't
// have renders as an empty cell). A "hash(...)" column (see
// sortHashFieldInner) renders its inner field's SHA-256 hex digest instead
// of its actual value.
func writeSortCSV(w io.Writer, header []string, rows []map[string]interface{}) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, row := range rows {
		cells := make([]string, len(header))
		for i, h := range header {
			cells[i] = sortValueAsString(resolveSortFieldOrHash(row, h))
		}
		if err := cw.Write(cells); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// ── JSON ─────────────────────────────────────────────────────────────────────

// readSortJSON decodes path and classifies its top-level shape (see
// unwrapJSONArray): either a flat array of row objects (rows, with wrapKey
// set if it was found wrapped in a single-key object), or a flat array of
// plain values (scalars, with scalarArray true) that --by/--fields don't
// apply to. json.Number is used throughout so a number round-trips exactly
// as written, rather than losing precision or trailing-zero formatting
// through float64.
func readSortJSON(path string) (wrapKey string, scalarArray bool, rows []map[string]interface{}, scalars []interface{}, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, nil, nil, err
	}
	var top interface{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return "", false, nil, nil, fmt.Errorf("parsing JSON: %w", err)
	}

	arr, wrapKey, err := unwrapJSONArray(top)
	if err != nil {
		return "", false, nil, nil, err
	}
	if len(arr) == 0 {
		return wrapKey, false, nil, nil, nil
	}
	if _, ok := arr[0].(map[string]interface{}); ok {
		rows = make([]map[string]interface{}, 0, len(arr))
		for i, el := range arr {
			m, ok := el.(map[string]interface{})
			if !ok {
				return "", false, nil, nil, fmt.Errorf("element %d is a %T, not a JSON object like the array's other elements", i, el)
			}
			rows = append(rows, m)
		}
		return wrapKey, false, rows, nil, nil
	}
	return wrapKey, true, nil, arr, nil
}

// unwrapJSONArray returns top itself if it's already a JSON array, or the
// single array-valued field of a one-key wrapping object — the
// {"rows": [...]} shape several 'bo' commands themselves output (also
// {"apps": [...]}, etc.) — returning that field's name as wrapKey so it can
// be restored on write. Anything else (not an array, an object with zero or
// more than one field, or a one-key object whose field isn't an array) isn't
// a flat list 'bo sort' can work with.
func unwrapJSONArray(top interface{}) (arr []interface{}, wrapKey string, err error) {
	switch v := top.(type) {
	case []interface{}:
		return v, "", nil
	case map[string]interface{}:
		if len(v) == 1 {
			for k, val := range v {
				if a, ok := val.([]interface{}); ok {
					return a, k, nil
				}
			}
		}
		return nil, "", fmt.Errorf(`top level is a JSON object, not an array of records — needs either a bare array or a single-key object wrapping one (e.g. {"rows": [...]})`)
	default:
		return nil, "", fmt.Errorf("top level is not a JSON array of records")
	}
}

// jsonLeafFields reports, across every row, which field names only ever hold
// a leaf (plain) value — string, number, bool, or null — never a nested
// object or array; leafOrder lists those names alphabetically (JSON object
// keys carry no reliable source order once decoded, so this mirrors the
// alphabetical column order 'bo' already uses elsewhere for dynamic,
// schema-less rows — see cmd/destflat.go's destColumnOrder).
func jsonLeafFields(rows []map[string]interface{}) (leafOrder []string, isLeaf map[string]bool) {
	isLeaf = make(map[string]bool)
	seen := make(map[string]bool)
	for _, row := range rows {
		for k, v := range row {
			leaf := isJSONLeafValue(v)
			if !seen[k] {
				seen[k] = true
				isLeaf[k] = leaf
			} else if !leaf {
				isLeaf[k] = false
			}
		}
	}
	for k, leaf := range isLeaf {
		if leaf {
			leafOrder = append(leafOrder, k)
		}
	}
	sort.Strings(leafOrder)
	return leafOrder, isLeaf
}

func isJSONLeafValue(v interface{}) bool {
	switch v.(type) {
	case map[string]interface{}, []interface{}:
		return false
	default:
		return true
	}
}

// filterJSONRow applies --fields to one row: a leaf field is kept only if
// fields is empty or names it; a non-leaf field (nested object/array) is
// always kept, since a plain (non-dotted, non-"hash(...)") --fields entry
// only ever selects among leaf fields. A dotted entry (e.g.
// "app_labels.commit_id") or a "hash(...)" entry (e.g. "hash(password)" —
// see sortHashFieldInner) is purely additive instead: it's resolved via
// resolveSortFieldOrHash and added as its own new top-level key, literally
// named after the entry itself, on top of whatever the plain rule above
// already decided — it never removes the named parent/inner field (e.g.
// "app_labels" or "password" itself, still kept whole per that rule, if
// also named plainly) or anything else.
func filterJSONRow(row map[string]interface{}, isLeaf map[string]bool, fields []string) map[string]interface{} {
	if len(fields) == 0 {
		return row
	}
	keep := make(map[string]bool, len(fields))
	var synthetic []string
	for _, f := range fields {
		if _, isHash := sortHashFieldInner(f); isHash || strings.Contains(f, ".") {
			synthetic = append(synthetic, f)
		} else {
			keep[f] = true
		}
	}
	out := make(map[string]interface{}, len(row)+len(synthetic))
	for k, v := range row {
		if isLeaf[k] && !keep[k] {
			continue
		}
		out[k] = v
	}
	for _, f := range synthetic {
		out[f] = resolveSortFieldOrHash(row, f)
	}
	return out
}

// writeSortJSON re-serializes rows (or scalars, for a bare array of plain
// values) back into the shape readSortJSON found — restoring wrapKey's
// wrapping object if one was found. Every object's keys come out in
// alphabetical order: encoding/json doesn't preserve (or accept) any other
// key order for a map, the same documented constraint as 'bo's other
// dynamic, schema-less JSON output (see cmd/destflat.go) — only a CSV
// file's column order can follow --fields' given order exactly.
func writeSortJSON(w io.Writer, wrapKey string, scalarArray bool, scalars []interface{}, rows []map[string]interface{}) error {
	var out interface{} = rows
	if scalarArray {
		out = scalars
	}
	if wrapKey != "" {
		out = map[string]interface{}{wrapKey: out}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ── per-file orchestration ──────────────────────────────────────────────────

// sortedFile holds one input file's final bytes, ready to write either back
// to its own path or to --output.
type sortedFile struct {
	path    string
	content []byte
}

// prepareSortedFile reads path (by its extension: .csv or .json), normalizes
// nestedFields' content (see applySortNestedFields), sorts its rows by
// byKeys (or, if empty, every available field in that format's natural
// order — see jsonLeafFields/readSortCSV), applies fields to narrow the
// output (see filterJSONRow for JSON; a plain column selection for CSV), and
// returns the resulting file content without writing it anywhere.
func prepareSortedFile(path string, byKeys []sortByKey, fields []string, nestedFields []string) (*sortedFile, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		header, rows, err := readSortCSV(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		applySortNestedFields(rows, nestedFields)
		keys := byKeys
		if len(keys) == 0 {
			keys = defaultSortKeys(fields, header)
		}
		sortRowsByKeys(rows, keys)
		outHeader := header
		if len(fields) > 0 {
			outHeader = fields
		}
		var buf bytes.Buffer
		if err := writeSortCSV(&buf, outHeader, rows); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return &sortedFile{path: path, content: buf.Bytes()}, nil

	case ".json":
		wrapKey, scalarArray, rows, scalars, err := readSortJSON(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		if scalarArray {
			if len(byKeys) > 0 || len(fields) > 0 {
				return nil, fmt.Errorf("%s: top level is a plain array of values, not records — --by/--fields don't apply", path)
			}
			sorted := append([]interface{}{}, scalars...)
			sort.SliceStable(sorted, func(i, j int) bool {
				return sortCompareValues(sorted[i], sorted[j]) < 0
			})
			var buf bytes.Buffer
			if err := writeSortJSON(&buf, wrapKey, true, sorted, nil); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			return &sortedFile{path: path, content: buf.Bytes()}, nil
		}

		applySortNestedFields(rows, nestedFields)
		leafOrder, isLeaf := jsonLeafFields(rows)
		keys := byKeys
		if len(keys) == 0 {
			keys = defaultSortKeys(fields, leafOrder)
		}
		sortedRows := append([]map[string]interface{}{}, rows...)
		sortRowsByKeys(sortedRows, keys)
		outRows := sortedRows
		if len(fields) > 0 {
			outRows = make([]map[string]interface{}, len(sortedRows))
			for i, r := range sortedRows {
				outRows[i] = filterJSONRow(r, isLeaf, fields)
			}
		}
		var buf bytes.Buffer
		if err := writeSortJSON(&buf, wrapKey, false, nil, outRows); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return &sortedFile{path: path, content: buf.Bytes()}, nil

	default:
		return nil, fmt.Errorf("%s: unsupported file extension %q — only .csv and .json are supported", path, filepath.Ext(path))
	}
}

// resolveSortFilePaths expands every pattern (shell-style glob: * ? [...],
// or a literal path) via filepath.Glob, in the order given, deduplicating
// matches already seen under an earlier pattern. A pattern matching nothing
// is only a warning — it's the caller's job to fail if the combined result
// is empty.
func resolveSortFilePaths(patterns []string, stderr io.Writer) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			fmt.Fprintf(stderr, "warning: invalid file pattern %q: %v\n", pattern, err)
			continue
		}
		if len(matches) == 0 {
			fmt.Fprintf(stderr, "warning: pattern %q matched no files\n", pattern)
			continue
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				paths = append(paths, m)
			}
		}
	}
	return paths
}

// ── command ──────────────────────────────────────────────────────────────────

var sortCmd = &cobra.Command{
	Use:   "sort <file_pattern> [file_pattern...]",
	Short: "Sort the rows of CSV/JSON file(s) on disk, in place or to --output",
	Long: `Sorts the rows of one or more CSV or JSON files already on disk — e.g. a
file a previous 'bo' command wrote with --output — rather than talking to
BTP/CF itself. Each file_pattern is a shell-style glob (*, ?, [...]) or a
literal path, expanded independently and combined (duplicates removed); a
pattern matching nothing is only a warning, not a failure, as long as at
least one file is matched overall. The file extension (.csv or .json)
selects how each matched file is read and written; anything else is an
error.

A CSV file's header row supplies its column names; a JSON file must decode
to either a flat array of objects ([{...}, {...}, ...]) or a single-key
object wrapping one (e.g. {"rows": [...]}, the shape several 'bo' commands
themselves write) — that wrapping key, if any, is kept on write. A bare
JSON array of plain values (not objects) is sorted directly; --by/--fields
don't apply to it.

Use --by/-b <field[:desc],...> to sort by one or more fields, in priority
order (ties on the first are broken by the next, and so on); suffix a field
with ":desc" for descending (":asc" is the default). Field names are matched
exactly (case-sensitive) and aren't validated — a field absent from one
file's data just compares as empty there, with no error, so one invocation
can cover several files with different columns/fields. Omit --by to sort
implicitly instead: by --fields' own fields, in the order given, if --fields
was also given (narrowing down to specific columns and then sorting by
exactly those is the common case) — otherwise by every available field,
ascending, in that format's natural order: a CSV file's header order, or —
since JSON object keys carry no reliable order once decoded — alphabetical
for JSON (the same convention 'bo' already uses elsewhere for dynamic,
schema-less rows). Each field's values are compared numerically when both
sides parse as a number, otherwise as plain text.

Use --fields/-f <field,...> to keep only those leaf fields in the output,
dropping every other leaf field. For CSV, every column is a leaf field, so
this is a plain column selector. For JSON, a "leaf" field is one that only
ever holds a plain value (string/number/bool/null) in the rows read — a
nested object or array is never a leaf field and is always kept regardless
of --fields, since --fields only ever selects among leaf fields. A CSV
column --fields names that a row doesn't actually have renders as an empty
cell; for JSON, naming a field that doesn't exist (or isn't a leaf) in a
given row simply has no effect on that row. Omit --fields to keep every
field. --fields' given order is only honored by CSV output (its header is
exactly the fields given, in that order) — a JSON object's keys always come
out alphabetical, since encoding/json doesn't preserve (or accept) any
other key order for a map; this is unrelated to --by, which sorts rows (not
fields) and is unaffected by --fields either way.

A field name containing a dot (e.g. "app_labels.commit_id", matching the
app_labels field 'bo apps --include-labels' writes) reaches into a nested
field, for both --fields and --by: the parent ("app_labels") is read, and
if its value is a JSON object — either already one, or a string holding
one, exactly how a CSV cell or a JSON-encoded field would carry it — the
remaining segment ("commit_id") is looked up as a key within it; several
dots chain this one segment at a time. The moment some segment's value
isn't a JSON object (or JSON-object-holding string), resolution stops
there and that segment's own whole value is used as-is instead — so
"app_labels.commit_id" against a plain, non-JSON app_labels value just
means "app_labels" itself, same as if no dot had been given. For --fields,
a dotted entry adds its own new field under that exact literal name
alongside whatever the plain rule above already decided — it never
replaces or drops the named parent field.

Wrap a --fields entry as "hash(field)" (e.g. "hash(password)", or
"hash(app_labels.commit_id)" — the inner name can be dotted too) to include
that field's SHA-256 hex digest instead of its actual value — a one-way
fingerprint, not the value itself, with no way to recover the original from
it. This lets a sensitive field (a password, client secret, etc.) still be
included and compared across two exports — the hash matches exactly when
the underlying value is exactly the same, and differs otherwise, which is
everything a diff needs — without ever writing the real value anywhere.
Exactly like a dotted entry, "hash(...)" is purely additive: it adds a new
field literally named "hash(password)" (not "password") alongside whatever
the plain rule above already decided, so the real "password" column/field
only appears if you separately name it too. An empty/absent field hashes to
an empty string rather than SHA-256('')'s own fixed hash, so a value that's
genuinely missing doesn't render as if something were there. --by (given
explicitly, or implied by --fields per the paragraph above) also
understands "hash(...)": it sorts by the hex digest itself, which still
never exposes the real value but does group rows sharing the same
underlying secret next to each other.

Use --sort-nested-fields <field,...> to also sort a field's own content into
a canonical order — not the rows themselves (see --by for that), but what's
inside the named field's value — so two otherwise-equivalent files that
only differ in some list's or object's internal ordering (e.g. tags written
in a different order, or a JSON object whose keys happened to be assembled
in a different order upstream) don't show up as a diff. Each name is a
plain, top-level field (no dotted paths). Its content is handled by shape:
a JSON array is sorted element by element (recursing into any nested
arrays/objects first); a JSON object has every one of its values recursed
into the same way (its own keys need no sorting — re-encoding a JSON object
already always emits its keys alphabetically); anything else is treated as
a plain delimited list and sorted as such — ',' whenever the value contains
one, otherwise ';' if that's what it uses instead (e.g. a tags field written
"a;b;c" to sidestep a CSV column's own comma), rejoined with that same
delimiter. This applies whether the
field holds that content natively (a JSON input file's own array/object) or
as a JSON-encoded string (e.g. a CSV cell, or a JSON field whose value is
itself a JSON-encoded string, both exactly the "app_labels" shape 'bo apps
--include-labels' writes) — either way the result is written back in the
same shape it was found. A field absent from a given row, or whose content
is a scalar or otherwise isn't a list/object at all, is left unchanged.
List/array elements are compared the same numeric-aware-else-text way --by
compares field values.

Without --output/-o, every matched file is sorted and overwritten in place:
unless -y is given, a confirmation listing every file about to be
overwritten is required first. --output instead writes the sorted result to
a different file, and only accepts exactly one matched input file (so it's
unambiguous which file the output came from).`,
	Args:    cobra.MinimumNArgs(1),
	GroupID: "utilities",
	RunE: func(cmd *cobra.Command, args []string) error {
		byFlag, _ := cmd.Flags().GetString("by")
		byKeys, err := parseSortByKeys(byFlag)
		if err != nil {
			return err
		}
		fieldsFlag, _ := cmd.Flags().GetString("fields")
		fields := parseSortFields(fieldsFlag)
		nestedFieldsFlag, _ := cmd.Flags().GetString("sort-nested-fields")
		nestedFields := parseSortFields(nestedFieldsFlag)
		outputFlag, _ := cmd.Flags().GetString("output")
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		paths := resolveSortFilePaths(args, cmd.ErrOrStderr())
		if len(paths) == 0 {
			return fmt.Errorf("no files matched any of the given patterns")
		}
		if outputFlag != "" && len(paths) != 1 {
			return fmt.Errorf("--output requires exactly one matching file, got %d", len(paths))
		}

		results := make([]*sortedFile, 0, len(paths))
		for _, p := range paths {
			r, err := prepareSortedFile(p, byKeys, fields, nestedFields)
			if err != nil {
				return err
			}
			results = append(results, r)
		}

		out := cmd.OutOrStdout()

		if outputFlag != "" {
			if err := os.WriteFile(outputFlag, results[0].content, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", outputFlag, err)
			}
			fmt.Fprintf(out, "Sorted %s -> %s.\n", results[0].path, outputFlag)
			return nil
		}

		if !skipConfirm {
			fmt.Fprintf(out, "This will overwrite %d file(s) in place:\n", len(results))
			for _, r := range results {
				fmt.Fprintf(out, "  %s\n", r.path)
			}
			fmt.Fprint(out, "Proceed? [y/N] ")
			text, ok := readLine(cmd.Context())
			if !ok || strings.ToLower(strings.TrimSpace(text)) != "y" {
				fmt.Fprintln(out, "Aborted.")
				return nil
			}
		}

		for _, r := range results {
			if err := os.WriteFile(r.path, r.content, 0o644); err != nil {
				return fmt.Errorf("writing %s: %w", r.path, err)
			}
		}
		fmt.Fprintf(out, "Sorted %d file(s) in place.\n", len(results))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(sortCmd)
	sortCmd.Flags().StringP("by", "b", "", "Comma-separated fields to sort by, in priority order, each optionally suffixed with \":desc\" (default \":asc\"); a dotted name (e.g. app_labels.commit_id) reaches into a nested field; omit to sort by --fields' own fields if given, else every available field, in order")
	sortCmd.Flags().StringP("fields", "f", "", "Comma-separated leaf field names to keep in the output, dropping every other leaf field (nested/non-leaf JSON fields are always kept); a dotted name (e.g. app_labels.commit_id) adds that nested field under its own literal name; wrap a name as hash(field) (e.g. hash(password)) to add its SHA-256 hex digest instead of its actual value, for comparing a sensitive field without exposing it; omit --fields to keep every field")
	sortCmd.Flags().String("sort-nested-fields", "", "Comma-separated, plain (non-dotted) field names whose own content — a ','- or ';'-delimited list, or a JSON array/object, either native or JSON-encoded as a string (e.g. a CSV cell) — is itself sorted into a canonical order, recursing into any nested arrays/objects; a field absent from a row, or whose content isn't a list/object, is left unchanged")
	sortCmd.Flags().StringP("output", "o", "", "Write the sorted result to this file instead of overwriting the input (only valid with exactly one matching file)")
	sortCmd.Flags().BoolP("yes", "y", false, "Skip the overwrite confirmation prompt")
}
