package googleworkspace

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
)

func TestIsNotConsistent_retryable(t *testing.T) {
	err := fmt.Errorf("something timed out while waiting")
	isRetryable := IsNotConsistent(err)
	if !isRetryable {
		t.Errorf("inconsistent error not detected as temporarily unavailable")
	}
}

func TestIsNotConsistent_other(t *testing.T) {
	err := googleapi.Error{
		Code: 404,
		Body: "some text describing error",
	}
	isRetryable := IsNotConsistent(&err)
	if isRetryable {
		t.Errorf("404 error detected as inconsistency error")
	}
}

func TestRetryTimeDurationReturnsNonRetryableErrorAfterRetryableError(t *testing.T) {
	previousMinTimeout := retryPollInterval
	retryPollInterval = time.Millisecond
	t.Cleanup(func() { retryPollInterval = previousMinTimeout })

	retryableErr := errors.New("timed out while waiting for propagation")
	nonRetryableErr := errors.New("permanent failure")
	attempts := 0
	err := retryTimeDuration(context.Background(), time.Second, func() error {
		attempts++
		if attempts == 1 {
			return retryableErr
		}
		return nonRetryableErr
	})
	if !errors.Is(err, nonRetryableErr) {
		t.Fatalf("error = %v, want non-retryable error %v", err, nonRetryableErr)
	}
}
