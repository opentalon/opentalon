package failover

import "fmt"

type ProviderError struct {
	StatusCode int
	Message    string
	Retryable  bool
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider error %d: %s", e.StatusCode, e.Message)
}

func IsRateLimitError(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.StatusCode == 429
	}
	return false
}

func IsAuthError(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.StatusCode == 401 || pe.StatusCode == 403
	}
	return false
}

func IsRetryable(err error) bool {
	if pe, ok := err.(*ProviderError); ok {
		return pe.Retryable || pe.StatusCode == 429 || pe.StatusCode == 500 || pe.StatusCode == 503
	}
	return false
}

type AllExhaustedError struct {
	Attempted []string
}

func (e *AllExhaustedError) Error() string {
	return fmt.Sprintf("all models exhausted, attempted: %v", e.Attempted)
}
