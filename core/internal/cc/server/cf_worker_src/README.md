Embedded copies of the relay Worker source (../../../../../cf-relay/src is
outside the package so go:embed cannot reach it directly).

Keep in sync: after editing cf-relay/src/*.js run

    cp cf-relay/src/*.js core/internal/cc/server/cf_worker_src/

The hot-migration deployer uploads these files verbatim via the CF Scripts
API; wrangler.toml (compat date, DO binding, migrations) is mirrored as
metadata in cf_deploy.go.
