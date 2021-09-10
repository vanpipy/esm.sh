package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"path"
	"strings"

	"github.com/ije/esbuild-internal/js_ast"
	"github.com/ije/esbuild-internal/js_parser"
	"github.com/ije/esbuild-internal/logger"
	"github.com/ije/esbuild-internal/test"
	"github.com/ije/gox/utils"
)

// ESM defines the ES Module meta
type ESM struct {
	*NpmPackage
	ExportDefault bool     `json:"exportDefault"`
	Exports       []string `json:"exports"`
	Dts           string   `json:"dts"`
}

func initESM(wd string, pkg pkg, checkExports bool, isDev bool) (esm *ESM, err error) {
	packageFile := path.Join(wd, "node_modules", pkg.name, "package.json")

	var p NpmPackage
	err = utils.ParseJSONFile(packageFile, &p)
	if err != nil {
		return
	}

	esm = &ESM{
		NpmPackage: fixNpmPackage(p),
	}

	if pkg.submodule != "" {
		packageFile := path.Join(wd, "node_modules", esm.Name, pkg.submodule, "package.json")
		if fileExists(packageFile) {
			var p NpmPackage
			err = utils.ParseJSONFile(packageFile, &p)
			if err != nil {
				return
			}
			if p.Main != "" {
				esm.Main = path.Join(pkg.submodule, p.Main)
			} else {
				esm.Main = ""
			}
			np := fixNpmPackage(p)
			if np.Module != "" {
				esm.Module = path.Join(pkg.submodule, np.Module)
			} else {
				esm.Module = ""
			}
			if p.Types != "" {
				esm.Types = path.Join(pkg.submodule, p.Types)
			} else {
				esm.Types = ""
			}
			if p.Typings != "" {
				esm.Typings = path.Join(pkg.submodule, p.Typings)
			} else {
				esm.Typings = ""
			}
		} else {
			var defined bool
			if p.DefinedExports != nil {
				if m, ok := p.DefinedExports.(map[string]interface{}); ok {
					for name, v := range m {
						/**
						exports: {
							"./lib/core": {
								"require": "./lib/core.js",
								"import": "./es/core.js"
							}
						}
						*/
						if name == "./"+pkg.submodule {
							useDefinedExports(esm.NpmPackage, v)
							defined = true
							break
							/**
							exports: {
								"./lib/languages/*": {
									"require": "./lib/languages/*.js",
									"import": "./es/languages/*.js"
								},
							}
							*/
						} else if strings.HasSuffix(name, "/*") && strings.HasPrefix("./"+pkg.submodule, strings.TrimSuffix(name, "*")) {
							suffix := strings.TrimPrefix("./"+pkg.submodule, strings.TrimSuffix(name, "*"))
							if m, ok := v.(map[string]interface{}); ok {
								for key, value := range m {
									s, ok := value.(string)
									if ok {
										m[key] = strings.Replace(s, "*", suffix, -1)
									}
								}
							}
							useDefinedExports(esm.NpmPackage, v)
							defined = true
						}
					}
				}
			}
			if !defined {
				if esm.Module != "" {
					esm.Module = pkg.submodule
				} else {
					esm.Main = pkg.submodule
				}
				esm.Types = ""
				esm.Typings = ""
			}
		}
	}

	if !checkExports {
		return
	}

	if esm.Module != "" {
		resolved, exportDefault, err := checkESM(wd, esm.Name, esm.Module)
		if err != nil {
			log.Warnf("fake module from '%s' of %s: %v", esm.Module, esm.Name, err)
			esm.Module = ""
		} else {
			esm.Module = resolved
			esm.ExportDefault = exportDefault
		}
	}

	if esm.Module == "" {
		nodeEnv := "production"
		if isDev {
			nodeEnv = "development"
		}
		ret, err := parseCJSModuleExports(wd, pkg.ImportPath(), nodeEnv)
		if err != nil {
			return nil, fmt.Errorf("parseCJSModuleExports: %v", err)
		}
		if strings.Contains(ret.Error, "Unexpected export statement in CJS module") {
			if pkg.submodule != "" {
				esm.Module = pkg.submodule
			} else {
				esm.Module = esm.Main
			}
			resolved, exportDefault, err := checkESM(wd, esm.Name, esm.Module)
			if err != nil {
				return nil, err
			}
			esm.Module = resolved
			esm.ExportDefault = exportDefault
		} else {
			esm.Exports = ret.Exports
			esm.ExportDefault = true
		}
	}
	return
}

func findESM(id string) (esm *ESM, pkgCSS bool, err error) {
	store, _, err := db.Get(id)
	if err == nil {
		err = json.Unmarshal([]byte(store["esm"]), &esm)
		if err != nil {
			db.Delete(id)
			return
		}

		var exists bool
		exists, err = fs.Exists(path.Join("builds", id))
		if !exists {
			db.Delete(id)
			return
		}

		if val := store["css"]; len(val) == 1 && val[0] == 1 {
			pkgCSS, err = fs.Exists(path.Join("builds", strings.TrimSuffix(id, ".js")+".css"))
		}
	}
	return
}

func checkESM(wd string, packageName string, moduleSpecifier string) (resolveName string, exportDefault bool, err error) {
	pkgDir := path.Join(wd, "node_modules", packageName)
	if dirExists(path.Join(pkgDir, moduleSpecifier)) {
		f := path.Join(moduleSpecifier, "index.mjs")
		if !fileExists(path.Join(pkgDir, f)) {
			f = path.Join(moduleSpecifier, "index.js")
		}
		moduleSpecifier = f
	}
	filename := path.Join(pkgDir, moduleSpecifier)
	switch path.Ext(filename) {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs":
	default:
		filename += ".js"
	}
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}
	log := logger.NewDeferLog(logger.DeferLogNoVerboseOrDebug)
	ast, pass := js_parser.Parse(log, test.SourceForTest(string(data)), js_parser.Options{})
	if pass {
		esm := ast.ExportsKind == js_ast.ExportsESM
		if !esm {
			err = errors.New("not a module")
			return
		}
		for name := range ast.NamedExports {
			if name == "default" {
				exportDefault = true
				break
			}
		}
	}
	resolveName = moduleSpecifier
	return
}
