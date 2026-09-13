// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package connector

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// These structural tests pin the small set of production wiring edges that the
// behavioral recovery tests cannot reach. LoadUserLogin is separated from its
// flap-run bookkeeping by rustpushgo.Connect, and the successful tail of
// IMClient.Connect is separated from its recovery workers and StatusKit startup
// by a live Rust client. The behavioral tests drive each helper directly, but a
// deletion at one of these call sites otherwise leaves them green while making
// recovery inert in production.
//
// Keep this suite semantic and narrow. It deliberately does not snapshot whole
// functions or harmless statement order: every asserted edge is a sole wire for
// a named recovery invariant, and ordering is asserted only where an intervening
// return or an early callback can silently strand the feature.

type parsedProduction struct {
	fset  *token.FileSet
	files []parsedGoFile
}

type parsedGoFile struct {
	file *ast.File
}

// productionSources parses every non-test Go file in deterministic filename
// order. Parsing and source discovery fail closed: a partial package must never
// make a wiring test pass by omission.
func productionSources(t *testing.T) parsedProduction {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read connector package directory: %v", err)
	}
	fset := token.NewFileSet()
	var files []parsedGoFile
	for _, entry := range entries { // os.ReadDir is sorted by filename.
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse production source %s: %v", name, err)
		}
		files = append(files, parsedGoFile{file: file})
	}
	if len(files) == 0 {
		t.Fatal("no production Go sources found; wiring checks cannot run")
	}
	return parsedProduction{fset: fset, files: files}
}

// findMethod requires exactly one concrete method declaration. Returning the
// first match would make duplicate or build-shape-dependent declarations pass
// according to map iteration order.
func findMethod(t *testing.T, src parsedProduction, recv, name string) *ast.FuncDecl {
	t.Helper()
	var matches []*ast.FuncDecl
	var locations []string
	for _, source := range src.files {
		for _, decl := range source.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != name || receiverTypeName(fn) != recv {
				continue
			}
			matches = append(matches, fn)
			locations = append(locations, src.fset.Position(fn.Pos()).String())
		}
	}
	if len(matches) != 1 {
		t.Fatalf("production method (*%s).%s declarations = %v, want exactly one", recv, name, locations)
	}
	if matches[0].Body == nil {
		t.Fatalf("production method (*%s).%s has no body", recv, name)
	}
	return matches[0]
}

func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

func enclosingFuncName(fn *ast.FuncDecl) string {
	if recv := receiverTypeName(fn); recv != "" {
		return fmt.Sprintf("(*%s).%s", recv, fn.Name.Name)
	}
	return fn.Name.Name
}

func selectorIs(expr ast.Expr, qualifier, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == qualifier
}

func callIs(call *ast.CallExpr, qualifier, name string) bool {
	if qualifier != "" {
		return selectorIs(call.Fun, qualifier, name)
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == name
	case *ast.Ident:
		return fun.Name == name
	default:
		return false
	}
}

func callsIn(node ast.Node, qualifier, name string) int {
	count := 0
	ast.Inspect(node, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && callIs(call, qualifier, name) {
			count++
		}
		return true
	})
	return count
}

func topLevelStatementsCalling(body *ast.BlockStmt, qualifier, name string) []int {
	var found []int
	for i, stmt := range body.List {
		if callsIn(stmt, qualifier, name) > 0 {
			found = append(found, i)
		}
	}
	return found
}

// directTopLevelGoCall finds a launch only when the whole top-level statement is
// `go qualifier.name(...)`. A launch nested in a conditional does not prove the
// epoch always starts its sole recovery worker.
func directTopLevelGoCall(body *ast.BlockStmt, qualifier, name string) (index int, call *ast.CallExpr, count int) {
	index = -1
	for i, stmt := range body.List {
		goStmt, ok := stmt.(*ast.GoStmt)
		if !ok || !callIs(goStmt.Call, qualifier, name) {
			continue
		}
		if index < 0 {
			index, call = i, goStmt.Call
		}
		count++
	}
	return
}

// productionMethodCallers lists every selector call with the feature method's
// name. These method names are package-private and unique; scanning all
// functions intentionally makes an unexpected free-function or foreign-receiver
// caller fail closed rather than disappear from the caller-set assertion.
func productionMethodCallers(src parsedProduction, method string) []string {
	var callers []string
	for _, source := range src.files {
		for _, decl := range source.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != method {
					return true
				}
				pos := src.fset.Position(call.Pos())
				callers = append(callers, fmt.Sprintf("%s:%d %s", pos.Filename, pos.Line, enclosingFuncName(fn)))
				return true
			})
		}
	}
	sort.Strings(callers)
	return callers
}

func requireOnlyMethodCaller(t *testing.T, src parsedProduction, method, want string) {
	t.Helper()
	callers := productionMethodCallers(src, method)
	if len(callers) != 1 || !strings.HasSuffix(callers[0], " "+want) {
		t.Fatalf("%s production receiver calls = %v, want exactly one in %s", method, callers, want)
	}
}

func productionCallers(src parsedProduction, qualifier, name string) []string {
	var callers []string
	for _, source := range src.files {
		for _, decl := range source.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !callIs(call, qualifier, name) {
					return true
				}
				pos := src.fset.Position(call.Pos())
				callers = append(callers, fmt.Sprintf("%s:%d %s", pos.Filename, pos.Line, enclosingFuncName(fn)))
				return true
			})
		}
	}
	sort.Strings(callers)
	return callers
}

func requireOnlyCaller(t *testing.T, src parsedProduction, qualifier, name, want string) {
	t.Helper()
	callers := productionCallers(src, qualifier, name)
	if len(callers) != 1 || !strings.HasSuffix(callers[0], " "+want) {
		t.Fatalf("%s production calls = %v, want exactly one in %s", name, callers, want)
	}
}

func exprIsSelector(expr ast.Expr, qualifier, name string) bool {
	return selectorIs(expr, qualifier, name)
}

func assignmentToSelector(assign *ast.AssignStmt, qualifier, field string) bool {
	if len(assign.Lhs) != 1 {
		return false
	}
	return exprIsSelector(assign.Lhs[0], qualifier, field)
}

func topLevelAssignmentIndex(body *ast.BlockStmt, qualifier, field string) int {
	found := -1
	for i, stmt := range body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || !assignmentToSelector(assign, qualifier, field) {
			continue
		}
		if found >= 0 {
			return -2
		}
		found = i
	}
	return found
}

func selectorAssignment(body *ast.BlockStmt, qualifier, field string) (rhs ast.Expr, index, count int, direct bool) {
	index = -1
	ast.Inspect(body, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || !assignmentToSelector(assign, qualifier, field) {
			return true
		}
		count++
		if len(assign.Rhs) != 1 {
			return true
		}
		rhs = assign.Rhs[0]
		for i, stmt := range body.List {
			if assign.Pos() < stmt.Pos() || assign.End() > stmt.End() {
				continue
			}
			index = i
			topAssign, ok := stmt.(*ast.AssignStmt)
			direct = ok && topAssign == assign
		}
		return true
	})
	return
}

func makeChanStruct(expr ast.Expr, capacity int, explicitCapacity bool) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	makeIdent, ok := call.Fun.(*ast.Ident)
	if !ok || makeIdent.Name != "make" || len(call.Args) < 1 || len(call.Args) > 2 {
		return false
	}
	chanType, ok := call.Args[0].(*ast.ChanType)
	if !ok || chanType.Dir != ast.SEND|ast.RECV {
		return false
	}
	st, ok := chanType.Value.(*ast.StructType)
	if !ok || st.Fields == nil || len(st.Fields.List) != 0 {
		return false
	}
	if explicitCapacity {
		if len(call.Args) != 2 {
			return false
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		return ok && lit.Kind == token.INT && lit.Value == fmt.Sprint(capacity)
	}
	return len(call.Args) == 1
}

func requireEpochStopArgument(t *testing.T, call *ast.CallExpr, worker string) {
	t.Helper()
	if len(call.Args) == 0 || !exprIsSelector(call.Args[0], "c", "stopChan") {
		t.Fatalf("IMClient.Connect must launch %s with c.stopChan as argument 1; got a different cancellation channel, so teardown would not retire this epoch's worker", worker)
	}
}

// The counting side of the flap run. A completed rustpushgo.Connect is the
// proof that a replacement APS ResourceManager exists. Consuming earlier counts
// a rebuild that may fail; consuming later permits an intervening return to
// leave a real replacement uncounted, permanently disabling widened backoff and
// repeated-rebuild StatusKit deferral.
func TestLoadUserLoginConsumesTheFlapRebuildRequestRightAfterConnect(t *testing.T) {
	src := productionSources(t)
	load := findMethod(t, src, "IMConnector", "LoadUserLogin")

	connectAt := topLevelStatementsCalling(load.Body, "rustpushgo", "Connect")
	if len(connectAt) != 1 || callsIn(load.Body, "rustpushgo", "Connect") != 1 {
		t.Fatalf("IMConnector.LoadUserLogin rustpushgo.Connect calls occur in top-level statements %v, want exactly one concrete construction", connectAt)
	}
	consumeAt := topLevelStatementsCalling(load.Body, "c", "consumeFlapRebuildRequest")
	if len(consumeAt) != 1 || callsIn(load.Body, "c", "consumeFlapRebuildRequest") != 1 {
		t.Fatalf("IMConnector.LoadUserLogin consumeFlapRebuildRequest calls occur in top-level statements %v, want exactly one; without it actual replacements never advance the flap run", consumeAt)
	}
	if consumeAt[0] != connectAt[0]+1 {
		t.Fatalf("IMConnector.LoadUserLogin must consume the flap rebuild request in the immediate next top-level statement after rustpushgo.Connect; Connect statement=%d consume statement=%d", connectAt[0], consumeAt[0])
	}
	requireOnlyMethodCaller(t, src, "consumeFlapRebuildRequest", "(*IMConnector).LoadUserLogin")
}

// The deciding side of StatusKit deferral. Connect must reach automatic startup
// only through launchOrDeferStatusKit; calling startStatusKit directly bypasses
// the repeated-flap decision, while deleting the decision leaves all helper
// behavior tests green and startup permanently absent.
func TestConnectRoutesStatusKitStartupThroughTheDeferralDecision(t *testing.T) {
	src := productionSources(t)
	connect := findMethod(t, src, "IMClient", "Connect")

	clientInstalledAt := topLevelAssignmentIndex(connect.Body, "c", "client")
	if clientInstalledAt < 0 {
		t.Fatalf("IMClient.Connect c.client installation index = %d, want exactly one top-level installation before StatusKit startup", clientInstalledAt)
	}
	decideAt := topLevelStatementsCalling(connect.Body, "c", "launchOrDeferStatusKit")
	if len(decideAt) != 1 || callsIn(connect.Body, "c", "launchOrDeferStatusKit") != 1 {
		t.Fatalf("IMClient.Connect launchOrDeferStatusKit calls occur in top-level statements %v, want exactly one; this is the sole decision that can set statusKitDeferred", decideAt)
	}
	if decideAt[0] <= clientInstalledAt {
		t.Fatalf("IMClient.Connect makes the StatusKit decision at statement %d before installing c.client at statement %d", decideAt[0], clientInstalledAt)
	}
	if n := callsIn(connect.Body, "", "startStatusKit"); n != 0 {
		t.Fatalf("IMClient.Connect calls startStatusKit directly %d times; all startup must route through launchOrDeferStatusKit", n)
	}
	requireOnlyMethodCaller(t, src, "launchOrDeferStatusKit", "(*IMClient).Connect")

	starters := productionMethodCallers(src, "startStatusKit")
	var names []string
	for _, caller := range starters {
		names = append(names, caller[strings.LastIndex(caller, " ")+1:])
	}
	sort.Strings(names)
	want := []string{"(*IMClient).activateDeferredStatusKit", "(*IMClient).launchOrDeferStatusKit"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("startStatusKit production receiver calls = %v, want exactly one from each of %v; any other caller bypasses deferral", starters, want)
	}
}

// newRustClient is the concrete FFI registration boundary. Its MessageCallback
// argument carries both OnMessage and OnConnectionEvent. The event-loop tests
// call OnConnectionEvent directly, so replacing this argument can leave every
// recovery test green while Rust has no route into the Go event latch.
func TestConnectRegistersItselfForAPSConnectionEventsBeforeRustCanCallback(t *testing.T) {
	src := productionSources(t)
	connect := findMethod(t, src, "IMClient", "Connect")

	newClientAt := topLevelStatementsCalling(connect.Body, "", "newRustClient")
	if len(newClientAt) != 1 || callsIn(connect.Body, "", "newRustClient") != 1 {
		t.Fatalf("IMClient.Connect newRustClient calls occur in top-level statements %v, want exactly one FFI client construction", newClientAt)
	}
	var newClientCall *ast.CallExpr
	ast.Inspect(connect.Body.List[newClientAt[0]], func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && callIs(call, "", "newRustClient") {
			newClientCall = call
			return false
		}
		return true
	})
	if newClientCall == nil || len(newClientCall.Args) != 7 {
		t.Fatalf("IMClient.Connect newRustClient argument count = %d, want 7 so MessageCallback identity can be verified", func() int {
			if newClientCall == nil {
				return -1
			}
			return len(newClientCall.Args)
		}())
	}
	requireOnlyCaller(t, src, "", "newRustClient", "(*IMClient).Connect")
	callback, ok := newClientCall.Args[5].(*ast.Ident)
	if !ok || callback.Name != "c" {
		t.Fatal("IMClient.Connect must pass c as newRustClient argument 6 (MessageCallback); that interface is Rust's sole production route to (*IMClient).OnConnectionEvent")
	}

	// The Rust drain task may publish an initial connection event before
	// newRustClient returns. A nil or unbuffered wake channel makes the callback's
	// nonblocking send disappear before the event loop exists.
	wakeRHS, wakeAt, wakeAssignments, wakeDirect := selectorAssignment(connect.Body, "c", "connectionEventWake")
	if wakeAssignments != 1 || wakeAt < 0 || !wakeDirect || !makeChanStruct(wakeRHS, 1, true) {
		t.Fatalf("IMClient.Connect connectionEventWake assignments = %d, want exactly one make(chan struct{}, 1)", wakeAssignments)
	}
	if wakeAt >= newClientAt[0] {
		t.Fatalf("IMClient.Connect initializes connectionEventWake at statement %d, want before newRustClient statement %d because Rust may callback before returning", wakeAt, newClientAt[0])
	}

	// Both workers receive c.stopChan by value. Prove that value is this
	// Connect's newly-created, unbuffered epoch channel rather than a stale/nil
	// field whose close can no longer cancel them.
	stopRHS, stopAt, stopAssignments, stopDirect := selectorAssignment(connect.Body, "c", "stopChan")
	if stopAssignments != 1 || stopAt < 0 || !stopDirect || !makeChanStruct(stopRHS, 0, false) {
		t.Fatalf("IMClient.Connect stopChan assignments = %d, want exactly one make(chan struct{}) for this epoch", stopAssignments)
	}
	if stopAt >= newClientAt[0] {
		t.Fatalf("IMClient.Connect initializes c.stopChan at statement %d, want before newRustClient statement %d", stopAt, newClientAt[0])
	}
}

// Both recovery workers are behavior-tested in isolation, but Connect is their
// only production launcher and its successful FFI tail cannot run in unit tests.
// The watchdog is the half-open/no-receive backstop and health-lease arbiter; it
// must start immediately after markConnected proves initialization succeeded, so
// no newly inserted early return can strand a connected epoch without recovery.
func TestConnectLaunchesTheReceiveWedgeWatchdogForTheEpoch(t *testing.T) {
	src := productionSources(t)
	connect := findMethod(t, src, "IMClient", "Connect")

	markAt := topLevelStatementsCalling(connect.Body, "c", "markConnected")
	if len(markAt) != 1 || callsIn(connect.Body, "c", "markConnected") != 1 {
		t.Fatalf("IMClient.Connect markConnected calls occur in top-level statements %v, want one successful-initialization boundary", markAt)
	}
	launchAt, call, direct := directTopLevelGoCall(connect.Body, "c", "runReceiveWedgeWatchdog")
	if total := callsIn(connect.Body, "c", "runReceiveWedgeWatchdog"); total != 1 || direct != 1 {
		t.Fatalf("IMClient.Connect runReceiveWedgeWatchdog calls: total=%d direct top-level go launches=%d, want exactly one unconditional epoch launch", total, direct)
	}
	if launchAt != markAt[0]+1 {
		t.Fatalf("IMClient.Connect must launch runReceiveWedgeWatchdog in the immediate next top-level statement after the markConnected success guard; markConnected statement=%d launch statement=%d", markAt[0], launchAt)
	}
	requireEpochStopArgument(t, call, "runReceiveWedgeWatchdog")
	requireOnlyMethodCaller(t, src, "runReceiveWedgeWatchdog", "(*IMClient).Connect")
}

// The APS event loop drains the callback latch and is the only path from Rust's
// connection events into outage/flap policy. It starts last because processing
// an event may synchronously tear the client down; launching it earlier races
// unfinished initialization. Deletion leaves callbacks latched forever.
func TestConnectLaunchesTheAPSConnectionEventLoopLastForTheEpoch(t *testing.T) {
	src := productionSources(t)
	connect := findMethod(t, src, "IMClient", "Connect")

	launchAt, call, direct := directTopLevelGoCall(connect.Body, "c", "runAPSConnectionEventLoop")
	if total := callsIn(connect.Body, "c", "runAPSConnectionEventLoop"); total != 1 || direct != 1 {
		t.Fatalf("IMClient.Connect runAPSConnectionEventLoop calls: total=%d direct top-level go launches=%d, want exactly one unconditional epoch launch", total, direct)
	}
	if launchAt != len(connect.Body.List)-1 {
		t.Fatalf("IMClient.Connect launches runAPSConnectionEventLoop at statement %d of %d, want it as the final top-level statement so an event cannot tear down partially initialized state", launchAt, len(connect.Body.List))
	}
	requireEpochStopArgument(t, call, "runAPSConnectionEventLoop")
	requireOnlyMethodCaller(t, src, "runAPSConnectionEventLoop", "(*IMClient).Connect")
}
