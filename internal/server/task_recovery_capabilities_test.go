package server

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"piccolod/internal/app"
	"piccolod/internal/app/catalog"
	"piccolod/internal/lifecycle"
)

func TestDecryptedTaskRecoveryOwnerOrderAndFreshAppBounds(t *testing.T) {
	srv := &GinServer{
		catalogManager: catalog.NewManager("", ""),
		updateManager:  &blockingUpdateManager{},
	}
	t.Cleanup(srv.catalogManager.Stop)

	type call struct {
		instanceID string
		ctx        context.Context
		remaining  time.Duration
	}
	var calls []call
	recoverApp := func(ctx context.Context, instanceID string) (app.AppRecoveryResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("app %s received no finite deadline", instanceID)
		}
		calls = append(calls, call{instanceID: instanceID, ctx: ctx, remaining: time.Until(deadline)})
		return app.AppRecoveryResult{InstanceID: instanceID, Recovered: true, RouteBearing: true, ActivePublication: true}, nil
	}

	owners := srv.decryptedTaskRecoveryOwners([]app.DesiredAppRecoveryOwner{
		{InstanceID: "zulu", RouteBearing: true},
		{InstanceID: "alpha", RouteBearing: true},
		{InstanceID: "zulu", RouteBearing: true},
		{},
	}, recoverApp)
	names := make([]string, len(owners))
	for i := range owners {
		names[i] = owners[i].Name
	}
	if want := []string{"app:alpha", "app:zulu", "catalog", "update"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("owner order = %v, want %v", names, want)
	}
	if owners[0].Timeout != taskRecoveryAppTimeout {
		t.Fatalf("first app ordinary timeout = %s, want %s", owners[0].Timeout, taskRecoveryAppTimeout)
	}
	if owners[1].Timeout != taskRecoveryAppTimeout {
		t.Fatalf("later app timeout = %s, want %s", owners[1].Timeout, taskRecoveryAppTimeout)
	}
	if owners[0].AttemptWithResult == nil || owners[1].AttemptWithResult == nil {
		t.Fatalf("fresh route-result attempts were not preserved: first=%v later=%v", owners[0].AttemptWithResult != nil, owners[1].AttemptWithResult != nil)
	}
	if owners[0].RouteQualification == nil || owners[0].RouteQualification.Timeout != taskRecoveryFirstRoute {
		t.Fatalf("first app qualification = %+v, want %s", owners[0].RouteQualification, taskRecoveryFirstRoute)
	}
	if owners[1].RouteQualification != nil {
		t.Fatalf("later app unexpectedly received route qualification: %+v", owners[1].RouteQualification)
	}

	attempts := []func(context.Context) (bool, error){
		owners[0].RouteQualification.Attempt,
		owners[1].Attempt,
	}
	for index, attempt := range attempts {
		active, err := attempt(context.Background())
		if err != nil || !active {
			t.Fatalf("attempt %d = active %v, err %v", index, active, err)
		}
	}
	if got := []string{calls[0].instanceID, calls[1].instanceID}; !reflect.DeepEqual(got, []string{"alpha", "zulu"}) {
		t.Fatalf("app closure calls = %v", got)
	}
	if calls[0].ctx == calls[1].ctx {
		t.Fatal("two app owners reused one operation context")
	}
	if calls[0].remaining <= 0 || calls[0].remaining > 5*time.Second {
		t.Fatalf("first app remaining bound = %s", calls[0].remaining)
	}
	if calls[1].remaining <= 0 || calls[1].remaining > taskRecoveryAppTimeout {
		t.Fatalf("later app remaining bound = %s", calls[1].remaining)
	}
}

func TestDecryptedTaskRecoveryOwnersQualifiesFirstRouteAheadOfListenerlessApps(t *testing.T) {
	var calls []string
	recoverApp := func(_ context.Context, instanceID string) (app.AppRecoveryResult, error) {
		calls = append(calls, instanceID)
		switch instanceID {
		case "alpha-workspace":
			return app.AppRecoveryResult{InstanceID: instanceID, Recovered: true}, nil
		case "beta-route":
			return app.AppRecoveryResult{InstanceID: instanceID, Recovered: true, RouteBearing: true, ActivePublication: true}, nil
		case "zulu-route":
			return app.AppRecoveryResult{InstanceID: instanceID, Recovered: true, RouteBearing: true, ActivePublication: true}, nil
		default:
			return app.AppRecoveryResult{}, nil
		}
	}

	owners := (&GinServer{}).decryptedTaskRecoveryOwners([]app.DesiredAppRecoveryOwner{
		{InstanceID: "zulu-route", RouteBearing: true},
		{InstanceID: "alpha-workspace"},
		{InstanceID: "beta-route", RouteBearing: true},
	}, recoverApp)
	gotNames := make([]string, len(owners))
	for index, owner := range owners {
		gotNames[index] = owner.Name
	}
	if want := []string{"app:beta-route", "app:alpha-workspace", "app:zulu-route"}; !reflect.DeepEqual(gotNames, want) {
		t.Fatalf("owner order = %v, want %v", gotNames, want)
	}
	if owners[0].Timeout != taskRecoveryAppTimeout {
		t.Fatalf("first route ordinary timeout = %s, want %s", owners[0].Timeout, taskRecoveryAppTimeout)
	}
	if owners[0].RouteQualification == nil || owners[0].RouteQualification.Timeout != taskRecoveryFirstRoute {
		t.Fatalf("first route qualification = %+v, want %s", owners[0].RouteQualification, taskRecoveryFirstRoute)
	}
	if owners[1].Timeout != taskRecoveryAppTimeout || owners[2].Timeout != taskRecoveryAppTimeout ||
		owners[1].RouteQualification != nil || owners[2].RouteQualification != nil {
		t.Fatalf("ordinary owner timeouts = %s/%s, want %s", owners[1].Timeout, owners[2].Timeout, taskRecoveryAppTimeout)
	}
	attempts := []func(context.Context) (bool, error){
		owners[0].RouteQualification.Attempt,
		owners[1].Attempt,
		owners[2].Attempt,
	}
	for index, attempt := range attempts {
		active, err := attempt(context.Background())
		if err != nil || !active {
			t.Fatalf("attempt %d = active %v, err %v", index, active, err)
		}
	}
	if want := []string{"beta-route", "alpha-workspace", "zulu-route"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("recovery calls = %v, want %v", calls, want)
	}
}

func TestDecryptedTaskRecoveryOwnerSeparatesRecoveryFromRoutePublication(t *testing.T) {
	tests := []struct {
		name                string
		owner               app.DesiredAppRecoveryOwner
		result              app.AppRecoveryResult
		ordinaryActive      bool
		routeActive         bool
		qualificationActive *bool
	}{
		{
			name:           "listenerless recovered without publication",
			owner:          app.DesiredAppRecoveryOwner{InstanceID: "workspace"},
			result:         app.AppRecoveryResult{InstanceID: "workspace", Recovered: true},
			ordinaryActive: true,
		},
		{
			name:                "route remains fail closed without publication",
			owner:               app.DesiredAppRecoveryOwner{InstanceID: "service", RouteBearing: true},
			result:              app.AppRecoveryResult{InstanceID: "service", Recovered: true, RouteBearing: true},
			qualificationActive: boolPointer(false),
		},
		{
			name:                "selected route becoming listenerless remains ordinary but does not qualify",
			owner:               app.DesiredAppRecoveryOwner{InstanceID: "changed", RouteBearing: true},
			result:              app.AppRecoveryResult{InstanceID: "changed", Recovered: true},
			ordinaryActive:      true,
			qualificationActive: boolPointer(false),
		},
		{
			name:   "listenerless becoming a route remains fail closed without publication",
			owner:  app.DesiredAppRecoveryOwner{InstanceID: "changed"},
			result: app.AppRecoveryResult{InstanceID: "changed", Recovered: true, RouteBearing: true},
		},
		{
			name:           "listenerless becoming a published route reports fresh route truth",
			owner:          app.DesiredAppRecoveryOwner{InstanceID: "published"},
			result:         app.AppRecoveryResult{InstanceID: "published", Recovered: true, RouteBearing: true, ActivePublication: true},
			ordinaryActive: true,
			routeActive:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			owners := (&GinServer{}).decryptedTaskRecoveryOwners([]app.DesiredAppRecoveryOwner{test.owner}, func(context.Context, string) (app.AppRecoveryResult, error) {
				return test.result, nil
			})
			if len(owners) != 1 {
				t.Fatalf("owners = %d, want 1", len(owners))
			}
			if owners[0].Timeout != taskRecoveryAppTimeout {
				t.Fatalf("ordinary timeout = %s, want %s", owners[0].Timeout, taskRecoveryAppTimeout)
			}
			active, err := owners[0].Attempt(context.Background())
			if err != nil || active != test.ordinaryActive {
				t.Fatalf("ordinary attempt = active %v, err %v, want active %v", active, err, test.ordinaryActive)
			}
			if owners[0].AttemptWithResult == nil {
				t.Fatal("app owner has no fresh route-result attempt")
			}
			fresh, err := owners[0].AttemptWithResult(context.Background())
			if err != nil || fresh.Active != test.ordinaryActive ||
				(fresh.RouteBearing && fresh.ActivePublication) != test.routeActive {
				t.Fatalf("fresh attempt = %+v err %v, want active=%v route-active=%v", fresh, err, test.ordinaryActive, test.routeActive)
			}
			if test.qualificationActive == nil {
				if owners[0].RouteQualification != nil {
					t.Fatalf("unexpected qualification: %+v", owners[0].RouteQualification)
				}
				return
			}
			if owners[0].RouteQualification == nil || owners[0].RouteQualification.Timeout != taskRecoveryFirstRoute {
				t.Fatalf("qualification = %+v, want %s", owners[0].RouteQualification, taskRecoveryFirstRoute)
			}
			active, err = owners[0].RouteQualification.Attempt(context.Background())
			if err != nil || active != *test.qualificationActive {
				t.Fatalf("qualification attempt = active %v, err %v, want active %v", active, err, *test.qualificationActive)
			}
		})
	}
}

func TestDecryptedTaskRecoveryOwnersReselectsFirstRouteAfterDurableShapeDrift(t *testing.T) {
	results := map[string]app.AppRecoveryResult{
		"alpha": {InstanceID: "alpha", Recovered: true},
		"beta":  {InstanceID: "beta", Recovered: true, RouteBearing: true, ActivePublication: true},
		"zulu":  {InstanceID: "zulu", Recovered: true, RouteBearing: true, ActivePublication: true},
	}
	recoverApp := func(_ context.Context, instanceID string) (app.AppRecoveryResult, error) {
		return results[instanceID], nil
	}

	initial := (&GinServer{}).decryptedTaskRecoveryOwners([]app.DesiredAppRecoveryOwner{
		{InstanceID: "zulu", RouteBearing: true},
		{InstanceID: "beta"},
		{InstanceID: "alpha", RouteBearing: true},
	}, recoverApp)
	if got, want := recoveryOwnerNames(initial), []string{"app:alpha", "app:beta", "app:zulu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("initial owner order = %v, want %v", got, want)
	}
	if initial[0].RouteQualification == nil {
		t.Fatal("initial route candidate has no qualification metadata")
	}
	if active, err := initial[0].RouteQualification.Attempt(context.Background()); err != nil || active {
		t.Fatalf("drifted initial qualification = active %v, err %v, want inactive", active, err)
	}
	if active, err := initial[0].Attempt(context.Background()); err != nil || !active {
		t.Fatalf("drifted initial ordinary attempt = active %v, err %v, want active", active, err)
	}

	refreshed := (&GinServer{}).decryptedTaskRecoveryOwners([]app.DesiredAppRecoveryOwner{
		{InstanceID: "zulu", RouteBearing: true},
		{InstanceID: "beta", RouteBearing: true},
		{InstanceID: "alpha"},
	}, recoverApp)
	if got, want := recoveryOwnerNames(refreshed), []string{"app:beta", "app:alpha", "app:zulu"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refreshed owner order = %v, want %v", got, want)
	}
	if refreshed[0].Timeout != taskRecoveryAppTimeout || refreshed[1].Timeout != taskRecoveryAppTimeout ||
		refreshed[0].RouteQualification == nil || refreshed[0].RouteQualification.Timeout != taskRecoveryFirstRoute {
		t.Fatalf("refreshed ordinary/qualification bounds = %s/%s/%+v, want %s/%s/%s",
			refreshed[0].Timeout, refreshed[1].Timeout, refreshed[0].RouteQualification,
			taskRecoveryAppTimeout, taskRecoveryAppTimeout, taskRecoveryFirstRoute)
	}
	if active, err := refreshed[0].RouteQualification.Attempt(context.Background()); err != nil || !active {
		t.Fatalf("reselected route qualification = active %v, err %v, want active", active, err)
	}
	if active, err := refreshed[1].Attempt(context.Background()); err != nil || !active {
		t.Fatalf("ordinary listenerless convergence = active %v, err %v, want active", active, err)
	}
}

func TestDecryptedTaskRecoveryOwnerActivityObservationFailsClosedWithoutAppManager(t *testing.T) {
	owners := (&GinServer{}).decryptedTaskRecoveryOwners(
		[]app.DesiredAppRecoveryOwner{{InstanceID: "alpha", RouteBearing: true}},
		func(context.Context, string) (app.AppRecoveryResult, error) {
			return app.AppRecoveryResult{InstanceID: "alpha", Recovered: true, RouteBearing: true, ActivePublication: true}, nil
		},
	)
	if len(owners) != 1 || owners[0].ObserveActive == nil {
		t.Fatalf("owners=%+v, want one app activity observer", owners)
	}
	active, err := owners[0].ObserveActive(context.Background())
	if active || err == nil {
		t.Fatalf("activity observation active=%v err=%v, want false/non-nil", active, err)
	}
}

func TestDecryptedTaskRecoveryNonAppOwnersKeepPointInTimeSuccessSemantics(t *testing.T) {
	catalogManager := catalog.NewManager("", "")
	t.Cleanup(catalogManager.Stop)
	owners := (&GinServer{catalogManager: catalogManager}).decryptedTaskRecoveryOwners(nil, nil)
	if len(owners) != 1 || owners[0].Name != "catalog" {
		t.Fatalf("owners=%+v, want catalog", owners)
	}
	if owners[0].ObserveActive != nil {
		t.Fatal("one-shot non-app owner unexpectedly acquired an app-style activity contract")
	}
}

func boolPointer(value bool) *bool { return &value }

func recoveryOwnerNames(owners []TaskRecoveryOwner) []string {
	names := make([]string, len(owners))
	for index := range owners {
		names[index] = owners[index].Name
	}
	return names
}

func TestTaskRecoveryStartCallbackIsSoleRecoveryAuthority(t *testing.T) {
	opCtx, opCancel := context.WithCancel(context.Background())
	t.Cleanup(opCancel)
	called := make(chan context.Context, 1)
	srv := &GinServer{
		opCtx:    opCtx,
		opCancel: opCancel,
	}
	srv.taskRecoveryStart = func(ctx context.Context, got *GinServer) {
		if got != srv {
			t.Errorf("callback server = %p, want %p", got, srv)
		}
		called <- ctx
	}

	srv.startOwnersAfterCoreReady()
	select {
	case gotCtx := <-called:
		if gotCtx != opCtx {
			t.Fatal("task recovery callback did not receive the server Stop context")
		}
	case <-time.After(time.Second):
		t.Fatal("external task recovery callback was not invoked")
	}
}

func TestPrepareAndReleaseTaskRecoveryApps(t *testing.T) {
	mgr, err := app.NewAppManagerForTest(nil, t.TempDir())
	if err != nil {
		t.Fatalf("app manager: %v", err)
	}
	mgr.ForceLockState(false)
	opCtx, opCancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		opCancel()
		mgr.StopBackground()
	})
	srv := &GinServer{
		appManager: mgr,
		lifecycle:  lifecycle.New(lifecycle.StateReady),
		opCtx:      opCtx,
		opCancel:   opCancel,
	}

	srv.PrepareTaskRecoveryApps([]string{"beta", "alpha", "beta"})
	if got := suppressedAppIDs(mgr); !reflect.DeepEqual(got, []string{"alpha", "beta"}) {
		t.Fatalf("suppressed apps = %v, want [alpha beta]", got)
	}

	srv.ReleaseTaskRecoveryApp("beta")
	if got := suppressedAppIDs(mgr); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("suppressed apps after beta release = %v, want [alpha]", got)
	}
	// Arbitrary release requests have no authority outside the prepared set.
	srv.ReleaseTaskRecoveryApp("not-prepared")
	if got := suppressedAppIDs(mgr); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Fatalf("suppressed apps after unrelated release = %v, want [alpha]", got)
	}
	srv.ReleaseTaskRecoveryApp("alpha")
	if got := suppressedAppIDs(mgr); len(got) != 0 {
		t.Fatalf("suppression retained after explicit owner release: %v", got)
	}
}

func suppressedAppIDs(mgr *app.AppManager) []string {
	snapshot := mgr.RuntimeObservationPressureSnapshot()
	ids := make([]string, 0, len(snapshot))
	for _, event := range snapshot {
		if event.ReasonCode == "automatic_recovery_suppressed" {
			ids = append(ids, event.AppInstanceID)
		}
	}
	sort.Strings(ids)
	return ids
}
