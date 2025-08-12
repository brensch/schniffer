package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/brensch/schniffer/internal/db"
)

func main() {
	var (
		query string
		file  string
		csv   bool
		duck  string
		args  multiFlag
		rw    bool
	)
	flag.StringVar(&query, "e", "", "SQL to execute (e.g. -e 'SELECT 1')")
	flag.StringVar(&file, "f", "", "SQL file to execute (read whole file)")
	flag.BoolVar(&csv, "csv", false, "Output results as CSV")
	flag.StringVar(&duck, "db", "", "Path to DuckDB file (defaults to DUCKDB_PATH env or ./schniffer.duckdb)")
	flag.Var(&args, "arg", "Query argument (repeatable). Example: -e 'SELECT ?' -arg 123")
	flag.BoolVar(&rw, "rw", false, "Open DB read-write (default is read-only)")
	flag.Parse()

	if duck == "" {
		duck = os.Getenv("DUCKDB_PATH")
		if duck == "" {
			duck = "./schniffer.duckdb"
		}
	}

	var (
		store *db.Store
		err   error
	)
	if rw {
		store, err = db.Open(duck)
	} else {
		fmt.Println("Opening DB in read-only mode...")
		store, err = db.OpenReadOnly(duck)
		if err != nil {
			fmt.Println("Failed to open read-only DB:", err)
			// Fallback: copy DB to a temp file to bypass locks and open read-only
			tmp, cerr := copyToTemp(duck)
			if cerr == nil {
				store, err = db.OpenReadOnly(tmp)
			}
		}
	}
	if err != nil {
		fatalf("open db: %v", err)
	}
	defer store.Close()

	sqlText := strings.TrimSpace(query)
	if sqlText == "" && file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			fatalf("read file: %v", err)
		}
		sqlText = strings.TrimSpace(string(b))
	}
	if sqlText == "" {
		// If nothing provided, read from stdin
		info, _ := os.Stdin.Stat()
		if (info.Mode() & os.ModeCharDevice) == 0 {
			in := bufio.NewReader(os.Stdin)
			var sb strings.Builder
			for {
				line, err := in.ReadString('\n')
				sb.WriteString(line)
				if err == io.EOF {
					break
				}
				if err != nil {
					fatalf("stdin: %v", err)
				}
			}
			sqlText = strings.TrimSpace(sb.String())
		}
	}
	if sqlText == "" {
		fatalf("no SQL provided. Use -e, -f, or pipe to stdin")
	}

	if looksLikeSelect(sqlText) {
		if err := runSelect(store.DB, sqlText, args.values(), csv); err != nil {
			fatalf("query failed: %v", err)
		}
		return
	}
	res, err := store.DB.ExecContext(context.Background(), sqlText, args.values()...)
	if err != nil {
		fatalf("exec failed: %v", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("OK (%d rows affected)\n", n)
}

func looksLikeSelect(q string) bool {
	s := strings.TrimSpace(strings.ToLower(q))
	return strings.HasPrefix(s, "select") || strings.HasPrefix(s, "with ")
}

func runSelect(dbx *sql.DB, q string, args []any, csv bool) error {
	rows, err := dbx.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if csv {
		// header
		fmt.Println(strings.Join(cols, ","))
	} else {
		fmt.Println(strings.Join(cols, " | "))
		fmt.Println(strings.Repeat("-", len(strings.Join(cols, "-+-"))))
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		out := make([]string, len(cols))
		for i, v := range vals {
			out[i] = fmtVal(v)
		}
		if csv {
			fmt.Println(strings.Join(out, ","))
		} else {
			fmt.Println(strings.Join(out, " | "))
		}
	}
	return rows.Err()
}

func fmtVal(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }
func (m *multiFlag) values() []any {
	out := make([]any, len(*m))
	for i, v := range *m {
		out[i] = v
	}
	return out
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

func copyToTemp(src string) (string, error) {
	// If src is relative, keep as-is; create temp file in same dir if possible
	b, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "schniffer-*.duckdb")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	return f.Name(), nil
}
