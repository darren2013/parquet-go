package parquet_test

import (
	"bytes"
	"cmp"
	"io"
	"math/rand"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/parquet-go/parquet-go"
)

func TestSortingWriter(t *testing.T) {
	type Row struct {
		Value int32 `parquet:"value"`
	}

	rows := make([]Row, 1000)
	for i := range rows {
		rows[i].Value = int32(i)
	}

	prng := rand.New(rand.NewSource(0))
	prng.Shuffle(len(rows), func(i, j int) {
		rows[i], rows[j] = rows[j], rows[i]
	})

	buffer := bytes.NewBuffer(nil)
	writer := parquet.NewSortingWriter[Row](buffer, 99,
		parquet.SortingWriterConfig(
			parquet.SortingColumns(
				parquet.Ascending("value"),
			),
		),
	)

	_, err := writer.Write(rows)
	if err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	read, err := parquet.Read[Row](bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(rows, func(a, b Row) int {
		return cmp.Compare(a.Value, b.Value)
	})

	assertRowsEqual(t, rows, read)
}

func TestSortingWriterDropDuplicatedRows(t *testing.T) {
	type Row struct {
		Value int32 `parquet:"value"`
	}

	rows := make([]Row, 1000)
	for i := range rows {
		rows[i].Value = int32(i / 2)
	}

	prng := rand.New(rand.NewSource(0))
	prng.Shuffle(len(rows), func(i, j int) {
		rows[i], rows[j] = rows[j], rows[i]
	})

	buffer := bytes.NewBuffer(nil)
	writer := parquet.NewSortingWriter[Row](buffer, 99,
		parquet.SortingWriterConfig(
			parquet.SortingBuffers(
				parquet.NewFileBufferPool("", "buffers.*"),
			),
			parquet.SortingColumns(
				parquet.Ascending("value"),
			),
			parquet.DropDuplicatedRows(true),
		),
	)

	_, err := writer.Write(rows)
	if err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	read, err := parquet.Read[Row](bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(rows, func(a, b Row) int {
		return cmp.Compare(a.Value, b.Value)
	})

	n := len(rows) / 2
	for i := range rows[:n] {
		rows[i] = rows[2*i]
	}

	assertRowsEqual(t, rows[:n], read)
}

func TestSortingWriterCorruptedString(t *testing.T) {
	type Row struct {
		Tag string `parquet:"tag"`
	}
	rowsWant := make([]Row, 107) // passes at 106, but fails at 107+
	for i := range rowsWant {
		rowsWant[i].Tag = randString(100)
	}

	buffer := bytes.NewBuffer(nil)

	writer := parquet.NewSortingWriter[Row](buffer, 2000,
		&parquet.WriterConfig{
			PageBufferSize: 2560,
			Sorting: parquet.SortingConfig{
				SortingColumns: []parquet.SortingColumn{
					parquet.Ascending("tag"),
				},
			},
		})

	_, err := writer.Write(rowsWant)
	if err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rowsGot, err := parquet.Read[Row](bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(rowsWant, func(a, b Row) int {
		return cmp.Compare(a.Tag, b.Tag)
	})

	assertRowsEqualByRow(t, rowsGot, rowsWant)
}

func TestSortingWriterCorruptedFixedLenByteArray(t *testing.T) {
	type Row struct {
		ID [16]byte `parquet:"id,uuid"`
	}
	rowsWant := make([]Row, 700) // passes at 300, fails at 400+.
	for i := range rowsWant {
		rowsWant[i].ID = rand16bytes()
	}

	buffer := bytes.NewBuffer(nil)

	writer := parquet.NewSortingWriter[Row](buffer, 2000,
		&parquet.WriterConfig{
			PageBufferSize: 2560,
			Sorting: parquet.SortingConfig{
				SortingColumns: []parquet.SortingColumn{
					parquet.Ascending("id"),
				},
			},
		})

	_, err := writer.Write(rowsWant)
	if err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	rowsGot, err := parquet.Read[Row](bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	if err != nil {
		t.Fatal(err)
	}

	slices.SortFunc(rowsWant, func(a, b Row) int {
		return bytes.Compare(a.ID[:], b.ID[:])
	})

	assertRowsEqualByRow(t, rowsGot, rowsWant)
}

const letterRunes = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func randString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterRunes[rand.New(rand.NewSource(time.Now().UnixNano())).Intn(len(letterRunes))]
	}
	return string(b)
}

func rand16bytes() [16]byte {
	var b [16]byte
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return b
}

func assertRowsEqualByRow[T any](t *testing.T, rowsGot, rowsWant []T) {
	if len(rowsGot) != len(rowsWant) {
		t.Errorf("want rows length %d but got rows length %d", len(rowsWant), len(rowsGot))
	}
	count := 0
	for i := range rowsGot {
		if !reflect.DeepEqual(rowsGot[i], rowsWant[i]) {
			t.Error("rows mismatch at index", i, ":")
			t.Logf(" want: %#v\n", rowsWant[i])
			t.Logf("  got: %#v\n", rowsGot[i])

			// check if rowsGot[i] is even present in rowsWant
			found := false
			for j := range rowsWant {
				if reflect.DeepEqual(rowsWant[j], rowsGot[i]) {
					t.Log("  we found the row at index", j, "in want.")
					found = true
					break
				}
			}
			if !found {
				t.Log("  got row index", i, "isn't found in want rows, and is therefore corrupted data.")
			}
			count++
		}
	}
	if count > 0 {
		t.Error(count, "rows mismatched out of", len(rowsWant), "total")
	}
}

func TestIssue82(t *testing.T) {
	type Record struct {
		A string `parquet:"a"`
	}

	fi, err := os.Open("testdata/lz4_raw_compressed_larger.parquet")
	if err != nil {
		t.Fatal(err)
	}
	defer fi.Close()

	stat, err := fi.Stat()
	if err != nil {
		t.Fatal(err)
	}

	fl, err := parquet.OpenFile(fi, stat.Size())
	if err != nil {
		t.Fatal(err)
	}
	groups := fl.RowGroups()
	if expect, got := 1, len(groups); expect != got {
		t.Fatalf("expected %d row groups got %d", expect, got)
	}

	fr := parquet.NewRowGroupReader(groups[0])

	var out bytes.Buffer

	pw := parquet.NewSortingWriter[Record](
		&out,
		1000,
		parquet.SortingWriterConfig(
			parquet.SortingColumns(parquet.Ascending("a")),
		),
	)

	if _, err := parquet.CopyRows(pw, fr); err != nil {
		t.Fatal(err)
	}

	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	rowsWant, err := parquet.Read[Record](fl, stat.Size())
	if err != nil {
		t.Fatal(err)
	}
	rowsGot, err := parquet.Read[Record](bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(rowsWant, func(a, b Record) int {
		return cmp.Compare(a.A, b.A)
	})
	assertRowsEqualByRow(t, rowsGot, rowsWant)
}

func TestMergedRowsCorruptedString(t *testing.T) {
	rowCount := 210 // starts failing at 210+
	type Row struct {
		Tag string `parquet:"tag"`
	}
	rowsWant := make([]Row, rowCount)
	for i := range rowsWant {
		rowsWant[i].Tag = randString(100)
	}

	// Create two files each with half of the rows.
	files := make([]*parquet.File, 2)
	for i := range 2 {
		buffer := bytes.NewBuffer(nil)

		writer := parquet.NewSortingWriter[Row](buffer, int64(rowCount),
			&parquet.WriterConfig{
				PageBufferSize: 2560,
				Sorting: parquet.SortingConfig{
					SortingColumns: []parquet.SortingColumn{
						parquet.Ascending("tag"),
					},
				},
			})

		_, err := writer.Write(rowsWant[i*(rowCount/2) : (i+1)*(rowCount/2)])
		if err != nil {
			t.Fatal(err)
		}

		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		f, err := parquet.OpenFile(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
		if err != nil {
			t.Fatal(err)
		}
		files[i] = f
	}

	// Merge the row groups from the separate files.
	merged, err := parquet.MergeRowGroups([]parquet.RowGroup{files[0].RowGroups()[0], files[1].RowGroups()[0]},
		parquet.SortingRowGroupConfig(parquet.SortingColumns(parquet.Ascending("tag"))),
	)
	if err != nil {
		t.Fatal(err)
	}
	if merged.NumRows() != int64(rowCount) {
		t.Fatal("number of rows mismatched: want", rowCount, "but got", merged.NumRows())
	}

	// Validate the merged rows.
	reader := merged.Rows()
	t.Cleanup(func() { reader.Close() })
	buf := make([]parquet.Row, rowCount)
	slices.SortFunc(rowsWant, func(a, b Row) int {
		return cmp.Compare(a.Tag, b.Tag)
	})
	for i, n := 0, 0; i < rowCount; i += n {
		n, err = reader.ReadRows(buf)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}

		for j, r := range buf[:n] {
			if rowsWant[i+j].Tag != r[0].String() {
				t.Fatalf("corruption at row %v: want %s but got %s", i+j, rowsWant[i+j].Tag, r[0].String())
			}
		}
	}
}

func TestIssue293(t *testing.T) {
	type Row struct {
		Value1 string `parquet:"value1"`
		Value2 string `parquet:"value2"`
		Value3 string `parquet:"value3"`
	}

	rows := make([]Row, 10)
	for i := range rows {
		rows[i].Value1 = strconv.Itoa(-i)
		rows[i].Value2 = strconv.Itoa(i)
		rows[i].Value3 = strconv.Itoa(i * i)
	}

	buffer := bytes.NewBuffer(nil)
	writer := parquet.NewSortingWriter[Row](buffer, 9,
		parquet.SkipPageBounds("value1"),
		parquet.SkipPageBounds("value3"),
		parquet.SortingWriterConfig(
			parquet.SortingColumns(
				parquet.Ascending("value1"),
				parquet.Descending("value2"),
			),
		),
	)

	if _, err := writer.Write(rows); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := parquet.OpenFile(bytes.NewReader(buffer.Bytes()), int64(buffer.Len()))
	if err != nil {
		t.Fatal(err)
	}

	bounds := [][2]string{
		{"", ""},
		{"0", "9"},
		{"", ""},
	}
	for i := range 3 {
		stats := f.Metadata().RowGroups[0].Columns[i].MetaData.Statistics

		min := string(stats.MinValue)
		if bounds[i][0] != min {
			t.Fatalf("wrong `min` value in column %d, expected %q, actual %q", i, bounds[i][0], min)
		}

		max := string(stats.MaxValue)
		if bounds[i][1] != max {
			t.Fatalf("wrong `max` value in column %d, expected %q, actual %q", i, bounds[i][1], max)
		}
	}
}

func TestEqualSortingColumns(t *testing.T) {
	tests := []struct {
		name     string
		a        []parquet.SortingColumn
		b        []parquet.SortingColumn
		expected bool
	}{
		{
			name:     "both empty",
			a:        []parquet.SortingColumn{},
			b:        []parquet.SortingColumn{},
			expected: true,
		},
		{
			name:     "both nil",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name:     "one empty, one nil",
			a:        []parquet.SortingColumn{},
			b:        nil,
			expected: true,
		},
		{
			name:     "different lengths",
			a:        []parquet.SortingColumn{parquet.Ascending("A")},
			b:        []parquet.SortingColumn{parquet.Ascending("A"), parquet.Descending("B")},
			expected: false,
		},
		{
			name:     "one empty, one non-empty",
			a:        []parquet.SortingColumn{},
			b:        []parquet.SortingColumn{parquet.Ascending("A")},
			expected: false,
		},
		{
			name:     "identical single column",
			a:        []parquet.SortingColumn{parquet.Ascending("A")},
			b:        []parquet.SortingColumn{parquet.Ascending("A")},
			expected: true,
		},
		{
			name: "identical multiple columns",
			a: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Descending("B"),
				parquet.Ascending("C"),
			},
			b: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Descending("B"),
				parquet.Ascending("C"),
			},
			expected: true,
		},
		{
			name:     "different column names",
			a:        []parquet.SortingColumn{parquet.Ascending("A")},
			b:        []parquet.SortingColumn{parquet.Ascending("B")},
			expected: false,
		},
		{
			name:     "different directions",
			a:        []parquet.SortingColumn{parquet.Ascending("A")},
			b:        []parquet.SortingColumn{parquet.Descending("A")},
			expected: false,
		},
		{
			name:     "different nulls first setting",
			a:        []parquet.SortingColumn{parquet.NullsFirst(parquet.Ascending("A"))},
			b:        []parquet.SortingColumn{parquet.Ascending("A")}, // defaults to nulls last
			expected: false,
		},
		{
			name:     "same nulls first setting",
			a:        []parquet.SortingColumn{parquet.NullsFirst(parquet.Ascending("A"))},
			b:        []parquet.SortingColumn{parquet.NullsFirst(parquet.Ascending("A"))},
			expected: true,
		},
		{
			name:     "complex paths identical",
			a:        []parquet.SortingColumn{parquet.Ascending("nested.field.a")},
			b:        []parquet.SortingColumn{parquet.Ascending("nested.field.a")},
			expected: true,
		},
		{
			name:     "complex paths different",
			a:        []parquet.SortingColumn{parquet.Ascending("nested.field.a")},
			b:        []parquet.SortingColumn{parquet.Ascending("nested.field.b")},
			expected: false,
		},
		{
			name: "multiple columns with difference in middle",
			a: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Ascending("B"), // different from b
				parquet.Descending("C"),
			},
			b: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Descending("B"), // different from a
				parquet.Descending("C"),
			},
			expected: false,
		},
		{
			name: "multiple columns with difference at end",
			a: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Descending("B"),
				parquet.Ascending("C"), // different from b
			},
			b: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Descending("B"),
				parquet.Descending("C"), // different from a
			},
			expected: false,
		},
		{
			name: "mixed properties - all same",
			a: []parquet.SortingColumn{
				parquet.NullsFirst(parquet.Ascending("A")),
				parquet.Descending("B"),
				parquet.NullsFirst(parquet.Descending("C")),
			},
			b: []parquet.SortingColumn{
				parquet.NullsFirst(parquet.Ascending("A")),
				parquet.Descending("B"),
				parquet.NullsFirst(parquet.Descending("C")),
			},
			expected: true,
		},
		{
			name: "mixed properties - different nulls handling",
			a: []parquet.SortingColumn{
				parquet.NullsFirst(parquet.Ascending("A")),
				parquet.Descending("B"),
			},
			b: []parquet.SortingColumn{
				parquet.Ascending("A"), // nulls last by default
				parquet.Descending("B"),
			},
			expected: false,
		},
		{
			name: "same columns in different order",
			a: []parquet.SortingColumn{
				parquet.Ascending("A"),
				parquet.Descending("B"),
			},
			b: []parquet.SortingColumn{
				parquet.Descending("B"),
				parquet.Ascending("A"),
			},
			expected: false, // Order matters
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := parquet.EqualSortingColumns(test.a, test.b)
			if result != test.expected {
				t.Errorf("EqualSortingColumns(%v, %v) = %v, expected %v",
					formatSortingColumns(test.a), formatSortingColumns(test.b), result, test.expected)
			}

			// Test symmetry - EqualSortingColumns should be commutative
			resultReverse := parquet.EqualSortingColumns(test.b, test.a)
			if result != resultReverse {
				t.Errorf("EqualSortingColumns is not symmetric: EqualSortingColumns(a, b) = %v, but EqualSortingColumns(b, a) = %v",
					result, resultReverse)
			}
		})
	}
}

// formatSortingColumns formats a slice of sorting columns for test output
func formatSortingColumns(cols []parquet.SortingColumn) string {
	if len(cols) == 0 {
		return "[]"
	}

	result := "["
	for i, col := range cols {
		if i > 0 {
			result += ", "
		}

		// Format each column with its properties
		path := col.Path()
		pathStr := ""
		if len(path) > 0 {
			pathStr = path[0]
			for _, p := range path[1:] {
				pathStr += "." + p
			}
		}

		direction := "asc"
		if col.Descending() {
			direction = "desc"
		}

		nulls := ""
		if col.NullsFirst() {
			nulls = "+nulls_first"
		}

		result += direction + "(" + pathStr + ")" + nulls
	}
	result += "]"
	return result
}
