package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/windshare/windshare/core/serviceauthority"
)

func (service *Service) call(ctx context.Context, route, bearer string, input, output any) error {
	return service.callWithTimeout(ctx, route, bearer, input, output, RequestTimeout)
}

func (service *Service) callWithTimeout(ctx context.Context, route, bearer string, input, output any, timeout time.Duration) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	requestContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost,
		strings.TrimRight(service.config.URL, "/")+route, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := service.config.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, serviceauthority.MaximumResponseBytes+1))
	if err != nil || len(data) > serviceauthority.MaximumResponseBytes {
		return errors.Join(serviceauthority.ErrFormat, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure fault
		if json.Unmarshal(data, &failure) == nil {
			switch {
			case response.StatusCode == http.StatusBadRequest && slices.Contains([]string{"invalid", "invalid_nickname", "invalid_email", "unsupported_email", "email_code_rejected"}, failure.Kind):
				return &failure
			case failure.Kind == "denied" && response.StatusCode == http.StatusForbidden:
				return &failure
			case failure.Kind == "verification" && response.StatusCode == http.StatusPreconditionRequired:
				return &failure
			case (failure.Kind == "rate_limited" || failure.Kind == "nickname_cooldown") && response.StatusCode == http.StatusTooManyRequests:
				return &failure
			case failure.Kind == "email_delivery_unavailable" && response.StatusCode == http.StatusServiceUnavailable:
				return &failure
			}
		}
		return fmt.Errorf("authority HTTP %d", response.StatusCode)
	}
	return json.Unmarshal(data, output)
}
