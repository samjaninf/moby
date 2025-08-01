package client

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/registry"
	"github.com/pkg/errors"
)

// PluginUpgrade upgrades a plugin
func (cli *Client) PluginUpgrade(ctx context.Context, name string, options PluginInstallOptions) (io.ReadCloser, error) {
	name, err := trimID("plugin", name)
	if err != nil {
		return nil, err
	}

	if err := cli.NewVersionError(ctx, "1.26", "plugin upgrade"); err != nil {
		return nil, err
	}
	query := url.Values{}
	if _, err := reference.ParseNormalizedNamed(options.RemoteRef); err != nil {
		return nil, errors.Wrap(err, "invalid remote reference")
	}
	query.Set("remote", options.RemoteRef)

	privileges, err := cli.checkPluginPermissions(ctx, query, options)
	if err != nil {
		return nil, err
	}

	resp, err := cli.tryPluginUpgrade(ctx, query, privileges, name, options.RegistryAuth)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (cli *Client) tryPluginUpgrade(ctx context.Context, query url.Values, privileges types.PluginPrivileges, name, registryAuth string) (*http.Response, error) {
	return cli.post(ctx, "/plugins/"+name+"/upgrade", query, privileges, http.Header{
		registry.AuthHeader: {registryAuth},
	})
}
