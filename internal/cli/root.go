package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const panBase = "https://pan.baidu.com"

type field struct {
	name     string
	required bool
	body     bool
	def      string
	help     string
}

type apiSpec struct {
	use      string
	short    string
	method   string
	base     string
	path     string
	query    url.Values
	fields   []field
	mutating bool
}

func New(version string) *cobra.Command {
	cfg := &config{}
	cmd := &cobra.Command{
		Use:           "bdpan",
		Short:         "Baidu Netdisk Open Platform CLI",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			envFallback(&cfg.appID, "BAIDU_APP_ID")
			envFallback(&cfg.appKey, "BAIDU_APP_KEY")
			envFallback(&cfg.secretKey, "BAIDU_SECRET_KEY")
			envFallback(&cfg.accessToken, "BAIDU_ACCESS_TOKEN")
			cfg.client = &http.Client{Timeout: cfg.timeout}
			return nil
		},
	}
	cmd.SetOut(os.Stdout)
	cmd.SetErr(os.Stderr)
	cmd.PersistentFlags().StringVar(&cfg.appID, "app-id", "", "Application AppID (or BAIDU_APP_ID)")
	cmd.PersistentFlags().StringVar(&cfg.appKey, "app-key", "", "Application AppKey (or BAIDU_APP_KEY)")
	cmd.PersistentFlags().StringVar(&cfg.secretKey, "secret-key", "", "Application SecretKey (or BAIDU_SECRET_KEY)")
	cmd.PersistentFlags().StringVar(&cfg.accessToken, "access-token", "", "Access token (or BAIDU_ACCESS_TOKEN)")
	cmd.PersistentFlags().StringVar(&cfg.tokenFile, "token-file", defaultTokenFile(), "Token file")
	cmd.PersistentFlags().DurationVar(&cfg.timeout, "timeout", 30*time.Second, "HTTP timeout")

	cmd.AddCommand(newAuthCmd(cfg), newUserCmd(cfg), newFileCmd(cfg))
	return cmd
}

func newUserCmd(cfg *config) *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "User and quota information"}
	cmd.AddCommand(
		newAPICommand(cfg, apiSpec{
			use: "info", short: "Get user information", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/nas", query: values("method", "uinfo"),
			fields: []field{{name: "vip_version", def: "v2", help: "Membership information version"}},
		}),
		newAPICommand(cfg, apiSpec{
			use: "quota", short: "Get netdisk quota information", method: http.MethodGet, base: panBase,
			path: "/api/quota",
			fields: []field{
				{name: "checkfree", def: "1", help: "Check free quota"},
				{name: "checkexpire", def: "1", help: "Check quota nearing expiration"},
			},
		}),
	)
	return cmd
}

func newFileCmd(cfg *config) *cobra.Command {
	cmd := &cobra.Command{Use: "file", Short: "Query, manage, and upload files"}
	cmd.AddCommand(
		newAPICommand(cfg, apiSpec{
			use: "list", short: "List files in a directory", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "list"),
			fields: fields(
				"dir", false, false, "/", "Absolute directory path",
				"num", false, false, "", "Items per page",
				"page", false, false, "", "Page number",
				"channel", false, false, "", "Channel",
				"clienttype", false, false, "", "Client type",
				"app_id", false, false, "", "AppID",
				"order", false, false, "name", "Sort by name/time/size",
				"desc", false, false, "0", "Set to 1 for descending order",
				"start", false, false, "0", "Start offset",
				"limit", false, false, "100", "Maximum number of results",
				"web", false, false, "0", "Return thumbnails",
				"folder", false, false, "0", "Return directories only",
				"showempty", false, false, "0", "Return whether directories are empty",
			),
		}),
		newAPICommand(cfg, apiSpec{
			use: "docs", short: "List documents", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "doclist"),
			fields: mediaListFields(),
		}),
		newAPICommand(cfg, apiSpec{
			use: "images", short: "List images", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "imagelist"),
			fields: mediaListFields(),
		}),
		newAPICommand(cfg, apiSpec{
			use: "search", short: "Search files by keyword", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "search"),
			fields: fields(
				"key", true, false, "", "Search keyword",
				"dir", false, false, "/", "Directory to search",
				"category", false, false, "", "1 video/2 audio/3 image/4 document/5 app/6 other/7 torrent",
				"num", false, false, "500", "Fixed at 500",
				"recursion", false, false, "", "Set a value to search recursively",
				"web", false, false, "", "Set a value to return thumbnails",
				"device_id", false, false, "", "Hardware device ID",
			),
		}),
		newAPICommand(cfg, apiSpec{
			use: "list-all", short: "List files recursively", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/multimedia", query: values("method", "listall"),
			fields: fields(
				"path", true, false, "", "Absolute application directory path",
				"recursion", false, false, "0", "Whether to recurse",
				"order", false, false, "name", "Sort by name/time/size",
				"desc", false, false, "0", "Set to 1 for descending order",
				"start", false, false, "0", "Start offset",
				"limit", false, false, "1000", "Maximum 10000",
				"ctime", false, false, "", "Minimum creation time",
				"mtime", false, false, "", "Minimum modification time",
				"web", false, false, "0", "Return thumbnails",
				"device_id", false, false, "", "Hardware device ID",
			),
		}),
		newAPICommand(cfg, apiSpec{
			use: "meta", short: "Get file metadata by path", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "meta"),
			fields: metadataFields("path"),
		}),
		newAPICommand(cfg, apiSpec{
			use: "metas-path", short: "Get metadata for multiple paths", method: http.MethodPost, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "filemetas"),
			fields: fields(
				"target", true, true, "", `JSON array of paths, for example ["/apps/app/a.txt"]`,
				"dlink", false, true, "0", "Return download links",
				"blocks", false, true, "0", "Return part MD5 hashes",
				"media", false, true, "0", "Return media information",
				"web", false, true, "0", "Return web fields",
			),
		}),
		newAPICommand(cfg, apiSpec{
			use: "metas", short: "Get metadata for multiple fs_id values", method: http.MethodGet, base: panBase,
			path: "/rest/2.0/xpan/multimedia", query: values("method", "filemetas"),
			fields: fields(
				"fsids", true, false, "", "JSON array of up to 100 fs_id values",
				"dlink", false, false, "0", "Return download links",
				"thumb", false, false, "0", "Return thumbnails",
				"extra", false, false, "0", "Return image information",
				"needmedia", false, false, "0", "Return video duration",
				"detail", false, false, "0", "Return video details",
				"path", false, false, "", "Shared directory or dedicated space path",
				"device_id", false, false, "", "Hardware device ID",
				"from_apaas", false, false, "", "Paid high-speed traffic entitlement",
			),
		}),
		newManagerCommand(cfg, "copy"),
		newManagerCommand(cfg, "move"),
		newManagerCommand(cfg, "rename"),
		newManagerCommand(cfg, "delete"),
		newAPICommand(cfg, apiSpec{
			use: "precreate", short: "Precreate an upload", method: http.MethodPost, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "precreate"), mutating: true,
			fields: fields(
				"path", true, true, "", "Absolute destination path",
				"size", true, true, "", "File size",
				"isdir", true, true, "0", "0 file/1 directory",
				"block_list", true, true, "", "JSON array of part MD5 hashes",
				"autoinit", true, true, "1", "Fixed at 1",
				"rtype", false, true, "1", "1/2 rename, 3 overwrite",
				"uploadid", false, true, "", "Existing upload ID",
				"content-md5", false, true, "", "File MD5",
				"slice-md5", false, true, "", "MD5 of the first 256 KB",
				"local_ctime", false, true, "", "Local creation time",
				"local_mtime", false, true, "", "Local modification time",
			),
		}),
		newUploadPartCmd(cfg),
		newAPICommand(cfg, apiSpec{
			use: "create", short: "Commit uploaded parts and create a file", method: http.MethodPost, base: panBase,
			path: "/rest/2.0/xpan/file", query: values("method", "create"), mutating: true,
			fields: fields(
				"path", true, true, "", "Absolute destination path",
				"size", true, true, "", "File size",
				"isdir", true, true, "0", "0 file/1 directory",
				"block_list", true, true, "", "JSON array of part MD5 hashes",
				"uploadid", true, true, "", "Upload ID returned by precreate",
				"rtype", false, true, "1", "Naming policy",
				"local_ctime", false, true, "", "Local creation time",
				"local_mtime", false, true, "", "Local modification time",
				"zip_quality", false, true, "", "Image compression quality",
				"zip_sign", false, true, "", "Original image MD5",
				"is_revision", false, true, "", "Enable file revisions",
				"mode", false, true, "", "Upload mode",
				"exif_info", false, true, "", "EXIF JSON",
			),
		}),
		newUploadCmd(cfg),
	)
	return cmd
}

func newManagerCommand(cfg *config, operation string) *cobra.Command {
	return newAPICommand(cfg, apiSpec{
		use: operation, short: map[string]string{"copy": "Copy files", "move": "Move files", "rename": "Rename files", "delete": "Delete files"}[operation],
		method: http.MethodPost, base: panBase, path: "/rest/2.0/xpan/file",
		query: values("method", "filemanager", "opera", operation), mutating: true,
		fields: fields(
			"async", true, true, "1", "0 synchronous/1 adaptive/2 asynchronous",
			"filelist", true, true, "", "JSON array of files to operate on",
			"ondup", false, true, "", "fail/newcopy/overwrite/skip",
		),
	})
}

func newAPICommand(cfg *config, spec apiSpec) *cobra.Command {
	valuesByName := make(map[string]*string, len(spec.fields))
	var yes bool
	cmd := &cobra.Command{
		Use:   spec.use,
		Short: spec.short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if spec.mutating && !yes {
				return errors.New("this command changes the netdisk; pass --yes to continue")
			}
			token, err := cfg.resolveAccessToken()
			if err != nil {
				return err
			}
			q := cloneValues(spec.query)
			q.Set("access_token", token)
			form := url.Values{}
			for _, f := range spec.fields {
				value := *valuesByName[f.name]
				if f.required && value == "" {
					return fmt.Errorf("--%s is required", flagName(f.name))
				}
				if value == "" {
					continue
				}
				if err := validateField(f.name, value); err != nil {
					return err
				}
				if f.body {
					form.Set(f.name, value)
				} else {
					q.Set(f.name, value)
				}
			}
			if spec.method != http.MethodPost {
				form = nil
			}
			return cfg.doJSON(cmd, spec.method, spec.base+spec.path, q, form, nil)
		},
	}
	for _, f := range spec.fields {
		value := f.def
		valuesByName[f.name] = &value
		cmd.Flags().StringVar(valuesByName[f.name], flagName(f.name), f.def, f.help)
	}
	if spec.mutating {
		cmd.Flags().BoolVar(&yes, "yes", false, "Confirm modifying the netdisk")
	}
	return cmd
}

func requireCredentials(cfg *config) error {
	if err := require("app-key", cfg.appKey); err != nil {
		return err
	}
	return require("secret-key", cfg.secretKey)
}

func require(name, value string) error {
	if value == "" {
		return fmt.Errorf("--%s or BAIDU_%s is required", name, strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
	}
	return nil
}

func validateField(name, value string) error {
	switch name {
	case "filelist", "block_list", "target", "fsids", "exif_info":
		if !json.Valid([]byte(value)) {
			return fmt.Errorf("--%s must be valid JSON", flagName(name))
		}
	case "rtype":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 3 {
			return errors.New("--rtype must be 0, 1, 2 or 3")
		}
	}
	return nil
}

func mediaListFields() []field {
	return fields(
		"parent_path", false, false, "/", "Absolute directory path",
		"page", false, false, "", "Page number",
		"num", false, false, "100", "Items per page",
		"order", false, false, "", "Sort by name/time/size",
		"desc", false, false, "", "0 ascending/1 descending",
		"recursion", false, false, "0", "Whether to recurse",
		"web", false, false, "0", "Return previews or thumbnails",
	)
}

func metadataFields(required string) []field {
	return fields(
		required, true, false, "", "Absolute file path",
		"dlink", false, false, "0", "Return download links",
		"thumb", false, false, "0", "Return thumbnails",
		"extra", false, false, "0", "Return image information",
		"needmedia", false, false, "0", "Return video information",
		"web", false, false, "0", "Return web fields",
	)
}

func fields(items ...any) []field {
	result := make([]field, 0, len(items)/5)
	for i := 0; i < len(items); i += 5 {
		result = append(result, field{
			name: items[i].(string), required: items[i+1].(bool), body: items[i+2].(bool),
			def: items[i+3].(string), help: items[i+4].(string),
		})
	}
	return result
}

func values(items ...string) url.Values {
	v := url.Values{}
	for i := 0; i < len(items); i += 2 {
		v.Set(items[i], items[i+1])
	}
	return v
}

func cloneValues(source url.Values) url.Values {
	result := url.Values{}
	for key, values := range source {
		result[key] = append([]string(nil), values...)
	}
	return result
}

func add(values url.Values, key, value string) {
	if value != "" {
		values.Set(key, value)
	}
}

func addInt(values url.Values, key string, value int) {
	if value != 0 {
		values.Set(key, strconv.Itoa(value))
	}
}

func flagName(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

func defaultTokenFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ".bdpan-token.json"
	}
	return filepath.Join(dir, "bdpan", "token.json")
}

func envFallback(target *string, name string) {
	if *target == "" {
		*target = os.Getenv(name)
	}
}
