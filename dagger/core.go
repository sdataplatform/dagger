package dagger

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tools "github.com/bhoriuchi/graphql-go-tools"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/printer"
	"github.com/moby/buildkit/client/llb"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
)

const (
	daggerSockName = "dagger-sock"
)

// FS is either llb representing the filesystem or a graphql query for obtaining that llb
// This is opaque to clients; to them FS is a scalar.
type FS struct {
	PB    *pb.Definition `json:"pb,omitempty"`
	Query string         `json:"query,omitempty"` // TODO: an actual graphql type would be better
}

func (fs FS) ToState() (llb.State, error) {
	if fs.PB == nil {
		return llb.State{}, fmt.Errorf("FS is not evaluated")
	}
	defop, err := llb.NewDefinitionOp(fs.PB)
	if err != nil {
		return llb.State{}, err
	}
	return llb.NewState(defop), nil
}

func (fs FS) Evaluate(ctx context.Context) (FS, error) {
	for fs.PB == nil {
		// TODO: guard against accidental infinite loop
		// this loop is where the "stack" is unwound, should later add debug info that traces each query leading to the final result
		if fs.Query == "" {
			return FS{}, fmt.Errorf("invalid fs: missing query")
		}
		result := graphql.Do(graphql.Params{
			Schema:        schema,
			RequestString: fs.Query,
			Context:       withEval(ctx),
		})
		if result.HasErrors() {
			return FS{}, fmt.Errorf("eval errors: %+v", result.Errors)
		}

		/* TODO: debugging
		// fmt.Printf("%T(%+v)\n", result.Data, result.Data)
		data, ok := result.Data.(map[string]interface{})["alpine"]
		if ok {
			// fmt.Printf("%T(%+v)\n", data, data)
			data, ok = data.(map[string]interface{})["build"]
			if ok {
				// fmt.Printf("%T(%+v)\n", data, data)
				data, ok = data.(map[string]interface{})["fs"]
				if ok {
					fmt.Printf("%T(%+v)\n", data, data)
				}
			}
		}
		*/

		// Extract the queried field out of the result
		// TODO: this is hilariously hacky, only looks for "fs", there obviously has to be a better way, just don't know where the graphql parsing utils are yet
		resultBytes, err := json.Marshal(result.Data)
		if err != nil {
			return FS{}, err
		}

		var resultMap map[string]interface{}
		if err := json.Unmarshal(resultBytes, &resultMap); err != nil {
			return FS{}, err
		}
		var found bool
		for !found {
			if len(resultMap) != 1 {
				return FS{}, fmt.Errorf("unhandled result: %+v", resultMap)
			}
			for k, v := range resultMap {
				if k == "fs" {
					if err := json.Unmarshal([]byte(v.(string)), &fs); err != nil {
						return FS{}, err
					}
					found = true
					break
				} else {
					resultMap = v.(map[string]interface{})
				}
			}
		}
	}
	return fs, nil
}

func (fs *FS) UnmarshalJSON(b []byte) error {
	// support marshaling from struct or from struct serialized to string (TODO: something's weird here with the graphql serialization, probably more sane way of doing this)
	var inner struct {
		PB    *pb.Definition `json:"pb,omitempty"`
		Query string         `json:"query,omitempty"`
	}
	if err := json.Unmarshal(b, &inner); err == nil {
		fs.PB = inner.PB
		fs.Query = inner.Query
		return nil
	}
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(str), &inner); err != nil {
		return err
	}
	fs.PB = inner.PB
	fs.Query = inner.Query
	return nil
}

var coreSchema tools.ExecutableSchema

type Image struct {
	FS FS `json:"fs"`
}

type Exec struct {
	FS FS `json:"fs"`
}

type Core struct {
	Image Image `json:"image"`
	Exec  Exec  `json:"exec"`
}

type CoreResult struct {
	Core Core `json:"core"`
}

type EvaluateResult struct {
	Evaluate FS `json:"evaluate"`
}

type ReadfileResult struct {
	Readfile string `json:"readfile"`
}

type DaggerPackage struct {
	Name string `json:"name"`
}

type evalKey struct{}

func withEval(ctx context.Context) context.Context {
	return context.WithValue(ctx, evalKey{}, true)
}

func shouldEval(ctx context.Context) bool {
	val, ok := ctx.Value(evalKey{}).(bool)
	return ok && val
}

/*
	type AlpineBuild {
		fs: FS!
	}
	type Query {
		build(pkgs: [String]!): AlpineBuild
	}

	converts to:

	type AlpineBuild {
		fs: FS!
	}
	type Alpine {
		build(pkgs: [String]!): AlpineBuild
	}
	type Query {
		alpine: Alpine!
	}
*/
func parseSchema(pkgName string, typeDefs string) (*tools.ExecutableSchema, error) {
	capName := strings.ToUpper(string(pkgName[0])) + pkgName[1:]
	resolverMap := tools.ResolverMap{
		"Query": &tools.ObjectResolver{
			Fields: tools.FieldResolveMap{
				pkgName: &tools.FieldResolve{
					Resolve: func(p graphql.ResolveParams) (interface{}, error) {
						return struct{}{}, nil
					},
				},
			},
		},
		capName: &tools.ObjectResolver{
			Fields: tools.FieldResolveMap{},
		},
	}

	doc, err := parser.Parse(parser.ParseParams{Source: typeDefs})
	if err != nil {
		return nil, err
	}

	var actions []string
	var otherObjects []string
	for _, def := range doc.Definitions {
		if def, ok := def.(*ast.ObjectDefinition); ok {
			if def.Name.Value == "Query" {
				for _, field := range def.Fields {
					actions = append(actions, printer.Print(field).(string))
					resolverMap[capName].(*tools.ObjectResolver).Fields[field.Name.Value] = &tools.FieldResolve{
						Resolve: actionFieldToResolver(pkgName, field.Name.Value),
					}
				}
			} else {
				otherObjects = append(otherObjects, printer.Print(def).(string))
			}
		}
	}

	return &tools.ExecutableSchema{
		TypeDefs: fmt.Sprintf(`
%s
type %s {
	%s
}
type Query {
	%s: %s!
}
	`, strings.Join(otherObjects, "\n"), capName, strings.Join(actions, "\n"), pkgName, capName),
		Resolvers: resolverMap,
	}, nil
}

func actionFieldToResolver(pkgName, actionName string) graphql.FieldResolveFn {
	return func(p graphql.ResolveParams) (interface{}, error) {
		if !shouldEval(p.Context) {
			lazyResult := make(map[string]interface{})
			// TODO: handle more return types, also handle nested objects
			for fieldName, field := range p.Info.ReturnType.(*graphql.Object).Fields() {
				if field.Type.Name() == "FS" || field.Type.Name() == "FS!" {
					// TODO: this is extra wrong, assumes that you are querying just this fs field
					lazyResult[fieldName] = FS{Query: getPayload(p.Context)}
				} else {
					lazyResult[fieldName] = struct{}{}
				}
			}
			return lazyResult, nil
		}

		imgref := fmt.Sprintf("localhost:5555/dagger:%s", pkgName)

		inputBytes, err := json.Marshal(p.Args)
		if err != nil {
			return nil, err
		}
		input := llb.Scratch().File(llb.Mkfile("/dagger.json", 0644, inputBytes))
		st := llb.Image(imgref).Run(
			llb.Args([]string{"/usr/local/bin/dagger", "-a", actionName}),
			llb.AddSSHSocket(
				llb.SSHID(daggerSockName),
				llb.SSHSocketTarget("/dagger.sock"),
			),
			llb.AddMount("/inputs", input, llb.Readonly),
			llb.ReadonlyRootFS(),
		)
		outputMnt := st.AddMount("/outputs", llb.Scratch())
		outputDef, err := outputMnt.Marshal(p.Context)
		if err != nil {
			return nil, err
		}

		gw, err := getGatewayClient(p.Context)
		if err != nil {
			return nil, err
		}
		res, err := gw.Solve(context.Background(), bkgw.SolveRequest{
			Evaluate:   true,
			Definition: outputDef.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		ref, err := res.SingleRef()
		if err != nil {
			return nil, err
		}
		outputBytes, err := ref.ReadFile(p.Context, bkgw.ReadRequest{
			Filename: "/dagger.json",
		})
		if err != nil {
			return nil, err
		}

		var result map[string]interface{}
		if err := json.Unmarshal(outputBytes, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
}

// TODO: shouldn't be global vars, pass through resolve context, make sure synchronization is handled, etc.
var schema graphql.Schema
var pkgSchemas map[string]tools.ExecutableSchema

func reloadSchemas() error {
	// tools.MakeExecutableSchema doesn't actually merge multiple schemas, it just overwrites any
	// overlapping types This is fine for the moment except for the Query type, which we need to be an
	// actual merge, so we do that ourselves here by merging the Query resolvers and appending a merged
	// Query type to the typeDefs.
	var queryFields []string
	var otherObjects []string
	for _, pkgSchema := range pkgSchemas {
		doc, err := parser.Parse(parser.ParseParams{Source: pkgSchema.TypeDefs})
		if err != nil {
			return err
		}
		for _, def := range doc.Definitions {
			if def, ok := def.(*ast.ObjectDefinition); ok {
				if def.Name.Value == "Query" {
					for _, field := range def.Fields {
						queryFields = append(queryFields, printer.Print(field).(string))
					}
					continue
				}
			}
			otherObjects = append(otherObjects, printer.Print(def).(string))
		}
	}

	resolvers := make(map[string]interface{})
	for _, pkgSchema := range pkgSchemas {
		for k, v := range pkgSchema.Resolvers {
			// TODO: need more general solution, verification that overwrites aren't happening, etc.
			if k == "Query" {
				if existing, ok := resolvers[k]; ok {
					existing := existing.(*tools.ObjectResolver)
					for field, fieldResolver := range v.(*tools.ObjectResolver).Fields {
						existing.Fields[field] = fieldResolver
					}
					v = existing
				}
			}
			resolvers[k] = v
		}
	}

	typeDefs := fmt.Sprintf(`
%s
type Query {
  %s
}
	`, strings.Join(otherObjects, "\n"), strings.Join(queryFields, "\n "))

	var err error
	schema, err = tools.MakeExecutableSchema(tools.ExecutableSchema{
		TypeDefs:  typeDefs,
		Resolvers: resolvers,
	})
	if err != nil {
		return err
	}

	return nil
}

func init() {
	pkgSchemas = make(map[string]tools.ExecutableSchema)
	pkgSchemas["core"] = tools.ExecutableSchema{
		TypeDefs: `
scalar FS

type CoreImage {
	fs: FS!
}
type CoreExec {
	fs: FS!
}

type Core {
	image(ref: String!): CoreImage
	exec(fs: FS!, args: [String]!): CoreExec
}
type Query {
	core: Core!
}

type Package {
	name: String!
}

type Mutation {
	import(ref: String!): Package
	evaluate(fs: FS!): FS
	readfile(fs: FS!, path: String!): String
}
		`,
		Resolvers: tools.ResolverMap{
			"Query": &tools.ObjectResolver{
				Fields: tools.FieldResolveMap{
					"core": &tools.FieldResolve{
						Resolve: func(p graphql.ResolveParams) (interface{}, error) {
							return struct{}{}, nil
						},
					},
				},
			},
			"Core": &tools.ObjectResolver{
				Fields: tools.FieldResolveMap{
					"image": &tools.FieldResolve{
						Resolve: func(p graphql.ResolveParams) (interface{}, error) {
							if !shouldEval(p.Context) {
								return Image{FS: FS{Query: getPayload(p.Context)}}, nil
							}
							ref, ok := p.Args["ref"].(string)
							if !ok {
								return nil, fmt.Errorf("invalid ref")
							}
							llbdef, err := llb.Image(ref).Marshal(p.Context)
							if err != nil {
								return nil, err
							}
							return Image{FS: FS{PB: llbdef.ToPB()}}, nil
						},
					},
					"exec": &tools.FieldResolve{
						Resolve: func(p graphql.ResolveParams) (interface{}, error) {
							if !shouldEval(p.Context) {
								return Exec{FS: FS{Query: getPayload(p.Context)}}, nil
							}
							fs, ok := p.Args["fs"].(FS)
							if !ok {
								return nil, fmt.Errorf("invalid fs")
							}
							rawArgs, ok := p.Args["args"].([]interface{})
							if !ok {
								return nil, fmt.Errorf("invalid args")
							}
							var args []string
							for _, arg := range rawArgs {
								if arg, ok := arg.(string); ok {
									args = append(args, arg)
								} else {
									return nil, fmt.Errorf("invalid arg")
								}
							}
							fs, err := fs.Evaluate(p.Context)
							if err != nil {
								return nil, err
							}
							fsState, err := fs.ToState()
							if err != nil {
								return nil, err
							}
							llbdef, err := fsState.Run(llb.Args(args)).Root().Marshal(p.Context)
							if err != nil {
								return nil, err
							}
							return Exec{FS: FS{PB: llbdef.ToPB()}}, nil
						},
					},
				},
			},

			"Mutation": &tools.ObjectResolver{
				Fields: tools.FieldResolveMap{
					"import": &tools.FieldResolve{
						Resolve: func(p graphql.ResolveParams) (interface{}, error) {
							// TODO: make sure not duped
							pkgName, ok := p.Args["ref"].(string)
							if !ok {
								return nil, fmt.Errorf("invalid ref")
							}
							imgref := fmt.Sprintf("localhost:5555/dagger:%s", pkgName)

							pkgDef, err := llb.Image(imgref).Marshal(p.Context)
							if err != nil {
								return nil, err
							}
							gw, err := getGatewayClient(p.Context)
							if err != nil {
								return nil, err
							}
							res, err := gw.Solve(context.Background(), bkgw.SolveRequest{
								Evaluate:   true,
								Definition: pkgDef.ToPB(),
							})
							if err != nil {
								return nil, err
							}
							bkref, err := res.SingleRef()
							if err != nil {
								return nil, err
							}
							outputBytes, err := bkref.ReadFile(p.Context, bkgw.ReadRequest{
								Filename: "/dagger.graphql",
							})
							if err != nil {
								return nil, err
							}
							parsed, err := parseSchema(pkgName, string(outputBytes))
							if err != nil {
								return nil, err
							}
							pkgSchemas[pkgName] = *parsed

							if err := reloadSchemas(); err != nil {
								return nil, err
							}
							return DaggerPackage{Name: pkgName}, nil
						},
					},
					"evaluate": &tools.FieldResolve{
						Resolve: func(p graphql.ResolveParams) (interface{}, error) {
							fs, ok := p.Args["fs"].(FS)
							if !ok {
								return nil, fmt.Errorf("invalid fs")
							}
							return fs.Evaluate(p.Context)
						},
					},
					"readfile": &tools.FieldResolve{
						Resolve: func(p graphql.ResolveParams) (interface{}, error) {
							fs, ok := p.Args["fs"].(FS)
							if !ok {
								return nil, fmt.Errorf("invalid fs")
							}
							path, ok := p.Args["path"].(string)
							if !ok {
								return nil, fmt.Errorf("invalid path")
							}
							fs, err := fs.Evaluate(p.Context)
							if err != nil {
								return nil, err
							}
							gw, err := getGatewayClient(p.Context)
							if err != nil {
								return nil, err
							}
							res, err := gw.Solve(context.Background(), bkgw.SolveRequest{
								Evaluate:   true,
								Definition: fs.PB,
							})
							if err != nil {
								return nil, err
							}
							ref, err := res.SingleRef()
							if err != nil {
								return nil, err
							}
							outputBytes, err := ref.ReadFile(p.Context, bkgw.ReadRequest{
								Filename: path,
							})
							if err != nil {
								return nil, err
							}
							return string(outputBytes), nil
						},
					},
				},
			},
			"FS": &tools.ScalarResolver{
				Serialize: func(value interface{}) interface{} {
					bytes, err := json.Marshal(value)
					if err != nil {
						panic(err)
					}
					return string(bytes)
				},
				ParseValue: func(value interface{}) interface{} {
					var fs FS
					var input string
					switch value := value.(type) {
					case string:
						input = value
					case *string:
						input = *value
					default:
						panic(fmt.Sprintf("unsupported type: %T", value))
					}
					if err := json.Unmarshal([]byte(input), &fs); err != nil {
						panic(err)
					}
					return fs
				},
				ParseLiteral: func(valueAST ast.Value) interface{} {
					switch valueAST := valueAST.(type) {
					case *ast.StringValue:
						var fs FS
						if err := json.Unmarshal([]byte(valueAST.Value), &fs); err != nil {
							panic(err)
						}
						return fs
					default:
						panic(fmt.Sprintf("unsupported type: %T", valueAST))
					}
				},
			},
		},
	}

	if err := reloadSchemas(); err != nil {
		panic(err)
	}
}
