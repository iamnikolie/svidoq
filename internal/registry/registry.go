// Package registry opens a configured schema as a concrete driver. It imports
// both the datasource contract and the drivers, keeping that knowledge out of
// the leaf packages.
package registry

import (
	"fmt"

	"github.com/iamnikolie/svidoq/internal/config"
	"github.com/iamnikolie/svidoq/internal/datasource"
	mysqldrv "github.com/iamnikolie/svidoq/internal/driver/mysql"
)

// Open resolves a schema in an environment and dials it.
func Open(cfg *config.Config, env, schema string) (datasource.Datasource, *config.Target, error) {
	target, err := cfg.Resolve(env, schema)
	if err != nil {
		return nil, nil, err
	}

	switch target.Driver {
	case "mysql", "mariadb":
		ds, err := mysqldrv.Open(target.Schema, target.DSN, config.ConnectTimeout)
		if err != nil {
			return nil, nil, err
		}

		return ds, target, nil
	default:
		return nil, nil, fmt.Errorf("unsupported driver %q (supported: mysql, mariadb)", target.Driver)
	}
}
