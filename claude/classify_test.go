package claude

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wolfeidau/durable"
)

func TestHeaderTimeoutIsTransientButCancellationIsNot(t *testing.T) {
	timeout := fmt.Errorf("timeout awaiting response headers: %w", context.DeadlineExceeded)
	transient := reflect.TypeOf(durable.Transient(errors.New("x")))

	require.Equal(t, transient, reflect.TypeOf(classify(context.Background(), timeout)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NotEqual(t, transient, reflect.TypeOf(classify(ctx, timeout)))
}
