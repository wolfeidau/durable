package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable/internal/audit"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
)

const sandboxName = "dt-test"

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func fakeGateway(ds []openshell.Decision, err error) audit.Gateway {
	return func(context.Context, string, time.Time) ([]openshell.Decision, error) {
		return ds, err
	}
}

func query(gw audit.Gateway, since time.Time) audit.Query {
	return audit.Query{Gateway: gw, Sandbox: sandboxName, Since: since}
}

func TestReadFromGateway(t *testing.T) {
	since := now.Add(-time.Hour)
	ds := []openshell.Decision{{Time: since.Add(10 * time.Minute), Host: "g.com"}}

	res, err := audit.Read(t.Context(), query(fakeGateway(ds, nil), since))

	require.NoError(t, err)
	require.Len(t, res.Events, 1)
	require.Equal(t, ds[0], res.Events[0].Decision)
	require.Equal(t, ds[0].Time, res.Coverage.GatewayStart, "the log starts after the window")
}

func TestReadGatewayReachesBackPastSince(t *testing.T) {
	since := now.Add(-time.Hour)
	gw := fakeGateway([]openshell.Decision{{Time: since.Add(-time.Minute), Host: "g.com"}}, nil)

	res, err := audit.Read(t.Context(), query(gw, since))

	require.NoError(t, err)
	require.True(t, res.Coverage.GatewayStart.IsZero())
}

func TestReadGatewayErrorIsCoverageNotFailure(t *testing.T) {
	boom := errors.New("gateway down")

	res, err := audit.Read(t.Context(), query(fakeGateway(nil, boom), now.Add(-time.Hour)))

	require.NoError(t, err)
	require.Empty(t, res.Events)
	require.ErrorIs(t, res.Coverage.GatewayErr, boom)
}

func TestReadWithoutGatewayReader(t *testing.T) {
	res, err := audit.Read(t.Context(), query(nil, now.Add(-time.Hour)))

	require.NoError(t, err)
	require.Error(t, res.Coverage.GatewayErr)
}

func TestReadEmpty(t *testing.T) {
	res, err := audit.Read(t.Context(), query(fakeGateway(nil, nil), now.Add(-time.Hour)))

	require.NoError(t, err)
	require.Empty(t, res.Events)
	require.NoError(t, res.Coverage.GatewayErr)
}
