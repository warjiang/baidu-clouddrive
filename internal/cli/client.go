package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type config struct {
	appID       string
	appKey      string
	secretKey   string
	accessToken string
	tokenFile   string
	timeout     time.Duration
	client      *http.Client
}

type token struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	SavedAt      int64  `json:"saved_at,omitempty"`
}

func tokenRequest(cmd *cobra.Command, cfg *config, endpoint string, q url.Values, save bool) error {
	body, err := cfg.request(cmd.Context(), http.MethodGet, endpoint, q, nil, nil)
	if err != nil {
		return err
	}
	if err := apiError(body); err != nil {
		return printResponse(cmd, body, err)
	}
	if save {
		if err := saveToken(cfg.tokenFile, body); err != nil {
			return err
		}
	}
	return printResponse(cmd, body, nil)
}

func (cfg *config) doJSON(cmd *cobra.Command, method, endpoint string, query, form url.Values, multipartBody func(*multipart.Writer) error) error {
	body, err := cfg.request(cmd.Context(), method, endpoint, query, form, multipartBody)
	if err != nil {
		return err
	}
	return printResponse(cmd, body, apiError(body))
}

func (cfg *config) request(ctx context.Context, method, endpoint string, query, form url.Values, multipartBody func(*multipart.Writer) error) ([]byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	u.RawQuery = query.Encode()
	var body io.Reader
	var contentType string
	if multipartBody != nil {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		if err := multipartBody(writer); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		body = &buffer
		contentType = writer.FormDataContentType()
	} else if form != nil {
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pan.baidu.com")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := cfg.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if apiError(data) != nil {
			return data, nil
		}
		return nil, fmt.Errorf("HTTP %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (cfg *config) resolveAccessToken() (string, error) {
	if cfg.accessToken != "" {
		return cfg.accessToken, nil
	}
	t, err := loadToken(cfg.tokenFile)
	if err != nil {
		return "", fmt.Errorf("access token not set and token file unavailable: %w", err)
	}
	if t.AccessToken == "" {
		return "", errors.New("access token is empty")
	}
	return t.AccessToken, nil
}

func saveToken(path string, data []byte) error {
	var t token
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	if t.AccessToken == "" {
		return errors.New("response contains no access token")
	}
	t.SavedAt = time.Now().Unix()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func loadToken(path string) (token, error) {
	var t token
	data, err := os.ReadFile(path)
	if err != nil {
		return t, err
	}
	err = json.Unmarshal(data, &t)
	return t, err
}

func printResponse(cmd *cobra.Command, data []byte, resultErr error) error {
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		fmt.Fprintln(cmd.OutOrStdout(), pretty.String())
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
	}
	return resultErr
}

func apiError(data []byte) error {
	var result map[string]any
	if json.Unmarshal(data, &result) != nil {
		return nil
	}
	if value, ok := number(result["errno"]); ok && value != 0 {
		return fmt.Errorf("API errno %d", value)
	}
	if value, ok := number(result["error_code"]); ok && value != 0 {
		return fmt.Errorf("OAuth error_code %d", value)
	}
	if value, ok := result["error"].(string); ok && value != "" {
		return fmt.Errorf("OAuth error: %s", value)
	}
	return nil
}

func number(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}
