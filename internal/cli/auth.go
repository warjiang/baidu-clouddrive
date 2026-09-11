package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/spf13/cobra"
)

const oauthBase = "https://openapi.baidu.com/oauth/2.0"

func newAuthCmd(cfg *config) *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "OAuth authorization"}
	cmd.AddCommand(
		newAuthURLCmd(cfg),
		newAuthExchangeCmd(cfg),
		newDeviceCodeCmd(cfg),
		newDeviceTokenCmd(cfg),
		newDeviceLoginCmd(cfg),
		newRefreshCmd(cfg),
	)
	return cmd
}

func newAuthURLCmd(cfg *config) *cobra.Command {
	var redirectURI, state, display, qrFrom string
	var qrcode, forceLogin, qrWidth, qrHeight int
	cmd := &cobra.Command{
		Use:   "url",
		Short: "Generate an authorization code flow URL",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := require("app-key", cfg.appKey); err != nil {
				return err
			}
			if redirectURI == "" {
				return errors.New("--redirect-uri is required")
			}
			q := url.Values{
				"response_type": {"code"},
				"client_id":     {cfg.appKey},
				"redirect_uri":  {redirectURI},
				"scope":         {"basic,netdisk"},
			}
			add(q, "device_id", cfg.appID)
			add(q, "state", state)
			add(q, "display", display)
			addInt(q, "qrcode", qrcode)
			add(q, "qrloginfrom", qrFrom)
			addInt(q, "qrcodeW", qrWidth)
			addInt(q, "qrcodeH", qrHeight)
			addInt(q, "force_login", forceLogin)
			fmt.Fprintln(cmd.OutOrStdout(), oauthBase+"/authorize?"+q.Encode())
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&redirectURI, "redirect-uri", "", "Redirect URI registered in the developer console")
	f.StringVar(&state, "state", "", "CSRF state")
	f.StringVar(&display, "display", "", "Authorization page display style")
	f.IntVar(&qrcode, "qrcode", 0, "Set to 1 to use QR code login")
	f.StringVar(&qrFrom, "qrloginfrom", "", "QR code style: watch/tv/kindle/speakers")
	f.IntVar(&qrWidth, "qrcode-width", 0, "QR code width")
	f.IntVar(&qrHeight, "qrcode-height", 0, "QR code height")
	f.IntVar(&forceLogin, "force-login", 0, "Set to 1 to force login")
	return cmd
}

func newAuthExchangeCmd(cfg *config) *cobra.Command {
	var code, redirectURI string
	var save bool
	cmd := &cobra.Command{
		Use:   "exchange",
		Short: "Exchange an authorization code for an access token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireCredentials(cfg); err != nil {
				return err
			}
			if code == "" || redirectURI == "" {
				return errors.New("--code and --redirect-uri are required")
			}
			q := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {code},
				"client_id":     {cfg.appKey},
				"client_secret": {cfg.secretKey},
				"redirect_uri":  {redirectURI},
			}
			return tokenRequest(cmd, cfg, oauthBase+"/token", q, save)
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "Code returned to the redirect URI")
	cmd.Flags().StringVar(&redirectURI, "redirect-uri", "", "Redirect URI used in the authorization URL")
	cmd.Flags().BoolVar(&save, "save", true, "Save the token")
	return cmd
}

func newDeviceCodeCmd(cfg *config) *cobra.Command {
	return &cobra.Command{
		Use:   "device-code",
		Short: "Request a device code and user code",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := require("app-key", cfg.appKey); err != nil {
				return err
			}
			q := url.Values{
				"response_type": {"device_code"},
				"client_id":     {cfg.appKey},
				"scope":         {"basic,netdisk"},
			}
			return cfg.doJSON(cmd, http.MethodGet, oauthBase+"/device/code", q, nil, nil)
		},
	}
}

func newDeviceTokenCmd(cfg *config) *cobra.Command {
	var code string
	var save bool
	cmd := &cobra.Command{
		Use:   "device-token",
		Short: "Exchange a device code for an access token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireCredentials(cfg); err != nil {
				return err
			}
			if code == "" {
				return errors.New("--code is required")
			}
			q := url.Values{
				"grant_type":    {"device_token"},
				"code":          {code},
				"client_id":     {cfg.appKey},
				"client_secret": {cfg.secretKey},
			}
			return tokenRequest(cmd, cfg, oauthBase+"/token", q, save)
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "Device Code")
	cmd.Flags().BoolVar(&save, "save", true, "Save the token")
	return cmd
}

func newDeviceLoginCmd(cfg *config) *cobra.Command {
	var save bool
	cmd := &cobra.Command{
		Use:   "device-login",
		Short: "Authorize with a device code and poll until complete",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireCredentials(cfg); err != nil {
				return err
			}
			q := url.Values{
				"response_type": {"device_code"},
				"client_id":     {cfg.appKey},
				"scope":         {"basic,netdisk"},
			}
			body, err := cfg.request(cmd.Context(), http.MethodGet, oauthBase+"/device/code", q, nil, nil)
			if err != nil {
				return err
			}
			var dc struct {
				DeviceCode      string `json:"device_code"`
				UserCode        string `json:"user_code"`
				VerificationURL string `json:"verification_url"`
				QRCodeURL       string `json:"qrcode_url"`
				ExpiresIn       int    `json:"expires_in"`
				Interval        int    `json:"interval"`
			}
			if err := json.Unmarshal(body, &dc); err != nil {
				return printResponse(cmd, body, err)
			}
			if dc.DeviceCode == "" {
				return printResponse(cmd, body, errors.New("device code missing"))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Visit %s and enter %s\nQR code: %s\n", dc.VerificationURL, dc.UserCode, dc.QRCodeURL)
			interval := time.Duration(max(dc.Interval, 3)) * time.Second
			deadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
			for time.Now().Before(deadline) {
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(interval):
				}
				tq := url.Values{
					"grant_type":    {"device_token"},
					"code":          {dc.DeviceCode},
					"client_id":     {cfg.appKey},
					"client_secret": {cfg.secretKey},
				}
				data, requestErr := cfg.request(cmd.Context(), http.MethodGet, oauthBase+"/token", tq, nil, nil)
				if requestErr != nil {
					return requestErr
				}
				var result map[string]any
				if err := json.Unmarshal(data, &result); err != nil {
					return printResponse(cmd, data, err)
				}
				if _, ok := result["access_token"]; ok {
					if save {
						if err := saveToken(cfg.tokenFile, data); err != nil {
							return err
						}
					}
					return printResponse(cmd, data, nil)
				}
				code, _ := result["error"].(string)
				if code != "authorization_pending" && code != "slow_down" {
					return printResponse(cmd, data, errors.New("device authorization failed"))
				}
				if code == "slow_down" {
					interval += 5 * time.Second
				}
			}
			return errors.New("device code expired")
		},
	}
	cmd.Flags().BoolVar(&save, "save", true, "Save the token")
	return cmd
}

func newRefreshCmd(cfg *config) *cobra.Command {
	var refreshToken string
	var save bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Refresh the access token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireCredentials(cfg); err != nil {
				return err
			}
			if refreshToken == "" {
				t, err := loadToken(cfg.tokenFile)
				if err != nil {
					return fmt.Errorf("--refresh-token not set and token file unavailable: %w", err)
				}
				refreshToken = t.RefreshToken
			}
			if refreshToken == "" {
				return errors.New("refresh token is required")
			}
			q := url.Values{
				"grant_type":    {"refresh_token"},
				"refresh_token": {refreshToken},
				"client_id":     {cfg.appKey},
				"client_secret": {cfg.secretKey},
			}
			return tokenRequest(cmd, cfg, oauthBase+"/token", q, save)
		},
	}
	cmd.Flags().StringVar(&refreshToken, "refresh-token", os.Getenv("BAIDU_REFRESH_TOKEN"), "Refresh Token")
	cmd.Flags().BoolVar(&save, "save", true, "Save the token")
	return cmd
}
