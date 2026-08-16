package sdk

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Every host capability must report itself when there is no Core, not crash.
//
// The clients are nil until Core hands over the reverse connection, so under a
// plugin's own `go test` every call has a nil receiver. That used to be a
// segmentation fault, which is a poor way to learn that a handler reached the
// store: the process dies and the test binary reports nothing useful.
//
// The guards are per-method, which is cheap but forgettable — nothing makes the
// next method added carry one. So this walks the methods with reflection rather
// than naming them: a new method with no guard fails here the first time it is
// added, without anyone remembering to extend a list.
//
// The alternative was a stub HostServicesClient, which the compiler would keep
// complete. I wrote that off in the guide as needing fake streams for Consume,
// Subscribe and Log — true of the stub, and irrelevant to the guards actually
// used, which return before touching a stream at all. The reason was about a
// design I was not using.
func TestEveryCapabilityReportsItselfWithoutACore(t *testing.T) {
	clients := []any{
		(*DBClient)(nil),
		(*TxClient)(nil),
		(*QueueClient)(nil),
		(*CacheClient)(nil),
		(*LockClient)(nil),
		(*FilesClient)(nil),
		(*EventClient)(nil),
		(*HTTPClient)(nil),
		(*Lease)(nil),
	}

	checked := 0
	for _, c := range clients {
		v := reflect.ValueOf(c)
		typ := v.Type()

		for i := range typ.NumMethod() {
			m := typ.Method(i)
			name := typ.Elem().Name() + "." + m.Name
			checked++

			// Where is the one method that reports through its return value
			// rather than an error: it hands back a Query carrying a deferred
			// error, so that a query can still be built and inspected with
			// Describe() when there is no Core. Checked below rather than
			// skipped, so the exception is verified instead of trusted.
			if name == "DBClient.Where" {
				continue
			}

			out, panicked := callWithZeroArgs(v.Method(i))
			if panicked != nil {
				t.Errorf("%s panicked on a nil client: %v\n"+
					"Add the same guard the others have: an unbound capability "+
					"returns ErrHostUnavailable so the author gets a sentence "+
					"rather than a dead process.", name, panicked)
				continue
			}
			assertUnavailable(t, name, out)
		}
	}

	assertWhereDefersItsError(t)

	// A sweep that reaches nothing reports the same thing as a sweep where
	// everything is in order. There are nine client types with at least one
	// exported method each; far fewer than that means the walk broke.
	if checked < 20 {
		t.Fatalf("only reached %d methods across %d clients; the reflection walk "+
			"is not finding them", checked, len(clients))
	}
	t.Logf("checked %d exported methods across %d capabilities", checked, len(clients))
}

// callWithZeroArgs invokes m with a zero value for every parameter. The guards
// return before looking at any of them, which is the property under test.
func callWithZeroArgs(m reflect.Value) (out []reflect.Value, panicked any) {
	defer func() { panicked = recover() }()

	typ := m.Type()
	args := make([]reflect.Value, 0, typ.NumIn())
	n := typ.NumIn()
	if typ.IsVariadic() {
		n-- // omit the variadic tail entirely
	}
	for i := range n {
		args = append(args, reflect.Zero(typ.In(i)))
	}
	return m.Call(args), nil
}

// assertUnavailable checks that the call reported ErrHostUnavailable. Methods
// returning no error at all are reported rather than skipped: a capability that
// cannot say it is unavailable is one whose failure is invisible.
func assertUnavailable(t *testing.T, name string, out []reflect.Value) {
	t.Helper()

	for _, o := range out {
		err, ok := o.Interface().(error)
		if !ok || err == nil {
			continue
		}
		if !errors.Is(err, ErrHostUnavailable) {
			t.Errorf("%s returned %v, want ErrHostUnavailable", name, err)
		}
		return
	}

	if returnsError(out) {
		t.Errorf("%s returned a nil error with no Core behind it, so a caller "+
			"would treat the call as having worked", name)
		return
	}
	t.Errorf("%s cannot report failure: it returns %s and no error", name, describe(out))
}

func returnsError(out []reflect.Value) bool {
	errType := reflect.TypeOf((*error)(nil)).Elem()
	for _, o := range out {
		if o.Type().Implements(errType) {
			return true
		}
	}
	return false
}

func describe(out []reflect.Value) string {
	if len(out) == 0 {
		return "nothing"
	}
	names := make([]string, len(out))
	for i, o := range out {
		names[i] = o.Type().String()
	}
	return fmt.Sprintf("(%s)", strings.Join(names, ", "))
}

// assertWhereDefersItsError covers the one capability that answers differently.
//
// A query has to be buildable without a Core — Describe() is how query
// construction is tested — so Where cannot fail up front. It returns a Query
// holding the error instead, and every terminal method reports it. All four are
// checked because two of them did not look at that field until recently: Count
// and aggregate ignored it while All and Rows honoured it, and nothing had ever
// set it, so the whole mechanism was dead and inconsistently wired at once.
func assertWhereDefersItsError(t *testing.T) {
	t.Helper()

	var db *DBClient
	q := db.Where("things").Eq("colour", "red").SortDesc("at").Limit(10)

	if got := q.Describe(); len(got.Filters) != 1 || got.Limit != 10 {
		t.Fatalf("the query did not build without a Core: %+v", got)
	}

	var dest []struct{}
	_, allErr := q.All(t.Context(), &dest)
	_, _, rowsErr := q.Rows(t.Context(), &dest)
	_, countErr := q.Count(t.Context())
	_, sumErr := q.Sum(t.Context(), "amount")

	for name, err := range map[string]error{
		"All": allErr, "Rows": rowsErr, "Count": countErr, "Sum": sumErr,
	} {
		if !errors.Is(err, ErrHostUnavailable) {
			t.Errorf("Query.%s returned %v, want ErrHostUnavailable", name, err)
		}
	}
}
