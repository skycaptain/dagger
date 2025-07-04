package dagql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sync"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/errcode"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/iancoleman/strcase"
	"github.com/opencontainers/go-digest"
	"github.com/sourcegraph/conc/pool"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/vektah/gqlparser/v2/parser"
	"github.com/vektah/gqlparser/v2/validator"
	"github.com/vektah/gqlparser/v2/validator/rules"
	"github.com/zeebo/xxh3"
	"golang.org/x/sync/errgroup"

	"github.com/dagger/dagger/dagql/call"
)

func init() {
	// HACK: these rules are disabled because some clients don't send the right
	// types:
	//   - PHP + Elixir SDKs send enums quoted
	//   - The shell sends enums quoted, and ints/floats as strings
	//   - etc
	validator.RemoveRule(rules.ValuesOfCorrectTypeRule.Name)
	validator.RemoveRule(rules.ValuesOfCorrectTypeRuleWithoutSuggestions.Name)

	// HACK: this rule is disabled because PHP modules <=v0.15.2 query
	// inputArgs incorrectly.
	validator.RemoveRule(rules.ScalarLeafsRule.Name)
}

// Server represents a GraphQL server whose schema is dynamically modified at
// runtime.
type Server struct {
	root       Object
	telemetry  AroundFunc
	objects    map[string]ObjectType
	scalars    map[string]ScalarType
	typeDefs   map[string]TypeDef
	directives map[string]DirectiveSpec

	schemas       map[View]*ast.Schema
	schemaDigests map[View]digest.Digest
	schemaOnces   map[View]*sync.Once
	schemaLock    *sync.Mutex

	installLock  *sync.Mutex
	installHooks []InstallHook

	// View is the default view that is applied to queries on this server.
	//
	// WARNING: this is *not* the view of the current query (for that, inspect
	// the current id)
	View View

	// Cache is the inner cache used by the server. It can be replicated to
	// another *Server to inherit and share caches.
	//
	// TODO: copy-on-write
	Cache *SessionCache
}

type InstallHook interface {
	InstallObject(ObjectType)
	// FIXME: add support for other install functions
}

// AroundFunc is a function that is called around every non-cached selection.
//
// It's a little funny looking. I may have goofed it. This will be cleaned up
// soon.
type AroundFunc func(
	context.Context,
	Object,
	*call.ID,
) (context.Context, func(res Typed, cached bool, err error))

// TypeDef is a type whose sole practical purpose is to define a GraphQL type,
// so it explicitly includes the Definitive interface.
type TypeDef interface {
	Type
	Definitive
}

// NewServer returns a new Server with the given root object.
func NewServer[T Typed](root T, c *SessionCache) *Server {
	srv := &Server{
		Cache:         c,
		objects:       map[string]ObjectType{},
		scalars:       map[string]ScalarType{},
		typeDefs:      map[string]TypeDef{},
		directives:    map[string]DirectiveSpec{},
		installLock:   &sync.Mutex{},
		schemas:       make(map[View]*ast.Schema),
		schemaDigests: make(map[View]digest.Digest),
		schemaOnces:   make(map[View]*sync.Once),
		schemaLock:    &sync.Mutex{},
	}
	rootClass := NewClass(srv, ClassOpts[T]{
		// NB: there's nothing actually stopping this from being a thing, except it
		// currently confuses the Dagger Go SDK. could be a nifty way to pass
		// around global config I suppose.
		NoIDs: true,
	})
	srv.root = Instance[T]{
		Self:  root,
		Class: rootClass,
	}
	srv.InstallObject(rootClass)
	for _, scalar := range coreScalars {
		srv.InstallScalar(scalar)
	}
	for _, directive := range coreDirectives {
		srv.InstallDirective(directive)
	}
	return srv
}

func (s *Server) invalidateSchemaCache() {
	s.schemaLock.Lock()
	clear(s.schemas)
	clear(s.schemaDigests)
	clear(s.schemaOnces)
	s.schemaLock.Unlock()
}

func NewDefaultHandler(es graphql.ExecutableSchema) *handler.Server {
	// TODO: avoid this deprecated method, and customize the options
	return handler.NewDefaultServer(es) //nolint: staticcheck
}

var coreScalars = []ScalarType{
	Boolean(false),
	Int(0),
	Float(0),
	String(""),
	// instead of a single ID type, each object has its own ID type
	// ID{},
}

var coreDirectives = []DirectiveSpec{
	{
		Name: "deprecated",
		Description: FormatDescription(
			`The @deprecated built-in directive is used within the type system
			definition language to indicate deprecated portions of a GraphQL
			service's schema, such as deprecated fields on a type, arguments on a
			field, input fields on an input type, or values of an enum type.`),
		Args: NewInputSpecs(
			InputSpec{
				Name: "reason",
				Description: FormatDescription(
					`Explains why this element was deprecated, usually also including a
					suggestion for how to access supported similar data. Formatted in
					[Markdown](https://daringfireball.net/projects/markdown/).`),
				Type:    String(""),
				Default: String("No longer supported"),
			},
		),
		Locations: []DirectiveLocation{
			DirectiveLocationFieldDefinition,
			DirectiveLocationArgumentDefinition,
			DirectiveLocationInputFieldDefinition,
			DirectiveLocationEnumValue,
		},
	},
	{
		Name: "experimental",
		Description: FormatDescription(
			`Explains why this element is marked experimental.
			Formatted in [Markdown](https://daringfireball.net/projects/markdown/).`),
		Args: NewInputSpecs(
			InputSpec{
				Name:        "reason",
				Description: FormatDescription(`Explains why this element was marked experimental.`),
				Type:        String(""),
				Default:     String("Not stabilized"),
			},
		),
		Locations: []DirectiveLocation{
			DirectiveLocationFieldDefinition,
			DirectiveLocationArgumentDefinition,
			DirectiveLocationInputFieldDefinition,
			DirectiveLocationEnumValue,
		},
	},
	{
		Name:        "sourceMap",
		Description: FormatDescription(`Indicates the source information for where a given field is defined.`),
		Args: NewInputSpecs(
			InputSpec{
				Name: "module",
				Type: String(""),
			},
			InputSpec{
				Name: "filename",
				Type: String(""),
			},
			InputSpec{
				Name: "line",
				Type: Int(0),
			},
			InputSpec{
				Name: "column",
				Type: Int(0),
			},
		),
		Locations: []DirectiveLocation{
			DirectiveLocationScalar,
			DirectiveLocationObject,
			DirectiveLocationFieldDefinition,
			DirectiveLocationArgumentDefinition,
			DirectiveLocationUnion,
			DirectiveLocationEnum,
			DirectiveLocationEnumValue,
			DirectiveLocationInputObject,
		},
	},
	{
		Name:        "enumValue",
		Description: FormatDescription(`Indicates the underlying value of an enum member.`),
		Args: NewInputSpecs(
			InputSpec{
				Name: "value",
				Type: String(""),
			},
		),
		Locations: []DirectiveLocation{
			DirectiveLocationEnumValue,
		},
	},
}

// Root returns the root object of the server. It is suitable for passing to
// Resolve to resolve a query.
func (s *Server) Root() Object {
	return s.root
}

type Loadable interface {
	Load(context.Context, *Server) (Typed, error)
}

// InstallObject installs the given Object type into the schema, or returns the
// previously installed type if it was already present
func (s *Server) InstallObject(class ObjectType) ObjectType {
	s.installLock.Lock()

	if class, ok := s.objects[class.TypeName()]; ok {
		s.installLock.Unlock()
		return class
	}

	s.invalidateSchemaCache()

	s.objects[class.TypeName()] = class
	if idType, hasID := class.IDType(); hasID {
		s.scalars[idType.TypeName()] = idType

		s.Root().ObjectType().Extend(
			FieldSpec{
				Name:        fmt.Sprintf("load%sFromID", class.TypeName()),
				Description: fmt.Sprintf("Load a %s from its ID.", class.TypeName()),
				Type:        class.Typed(),
				Args: NewInputSpecs(
					InputSpec{
						Name: "id",
						Type: idType,
					},
				),
			},
			func(ctx context.Context, self Object, args map[string]Input) (Typed, error) {
				idable, ok := args["id"].(IDable)
				if !ok {
					return nil, fmt.Errorf("expected IDable, got %T", args["id"])
				}
				id := idable.ID()
				if id.Type().ToAST().NamedType != class.TypeName() {
					return nil, fmt.Errorf("expected ID of type %q, got %q", class.TypeName(), id.Type().ToAST().NamedType)
				}
				res, err := s.Load(ctx, idable.ID())
				if err != nil {
					return nil, fmt.Errorf("load: %w", err)
				}
				return res, nil
			},
			CacheSpec{
				DoNotCache: "There's no point caching the loading call of an ID vs. letting the ID's calls cache on their own.",
			},
		)
	}
	s.installLock.Unlock()

	for _, hook := range s.installHooks {
		hook.InstallObject(class)
	}

	return class
}

// InstallScalar installs the given Scalar type into the schema, or returns the
// previously installed type if it was already present
func (s *Server) InstallScalar(scalar ScalarType) ScalarType {
	s.installLock.Lock()
	defer s.installLock.Unlock()
	if scalar, ok := s.scalars[scalar.TypeName()]; ok {
		return scalar
	}
	s.invalidateSchemaCache()
	s.scalars[scalar.TypeName()] = scalar
	return scalar
}

// InstallDirective installs the given Directive type into the schema.
func (s *Server) InstallDirective(directive DirectiveSpec) {
	s.installLock.Lock()
	defer s.installLock.Unlock()
	s.directives[directive.Name] = directive
	s.invalidateSchemaCache()
}

// InstallTypeDef installs an arbitrary type definition into the schema.
func (s *Server) InstallTypeDef(def TypeDef) {
	s.installLock.Lock()
	defer s.installLock.Unlock()
	s.typeDefs[def.TypeName()] = def
	s.invalidateSchemaCache()
}

// ObjectType returns the ObjectType with the given name, if it exists.
func (s *Server) ObjectType(name string) (ObjectType, bool) {
	s.installLock.Lock()
	defer s.installLock.Unlock()
	t, ok := s.objects[name]
	return t, ok
}

// ScalarType returns the ScalarType with the given name, if it exists.
func (s *Server) ScalarType(name string) (ScalarType, bool) {
	s.installLock.Lock()
	defer s.installLock.Unlock()
	t, ok := s.scalars[name]
	return t, ok
}

// InputType returns the InputType with the given name, if it exists.
func (s *Server) TypeDef(name string) (TypeDef, bool) {
	s.installLock.Lock()
	defer s.installLock.Unlock()
	t, ok := s.typeDefs[name]
	return t, ok
}

// Around installs a function to be called around every non-cached selection.
func (s *Server) Around(rec AroundFunc) {
	s.telemetry = rec
}

// Query is a convenience method for executing a query against the server
// without having to go through HTTP. This can be useful for introspection, for
// example.
func (s *Server) Query(ctx context.Context, query string, vars map[string]any) (map[string]any, error) {
	return s.ExecOp(ctx, &graphql.OperationContext{
		RawQuery:  query,
		Variables: vars,
	})
}

var _ graphql.ExecutableSchema = (*Server)(nil)

// Schema returns the current schema of the server.
func (s *Server) Schema() *ast.Schema {
	s.schemaLock.Lock()
	defer s.schemaLock.Unlock()

	view := s.View
	if s.schemaOnces[view] == nil {
		s.schemaOnces[view] = &sync.Once{}
	}

	s.schemaOnces[view].Do(func() {
		queryType := s.Root().Type().Name()
		schema := &ast.Schema{
			Types:         make(map[string]*ast.Definition),
			PossibleTypes: make(map[string][]*ast.Definition),
		}
		for _, t := range s.objects { // TODO stable order
			def := definition(ast.Object, t, view)
			if def.Name == queryType {
				schema.Query = def
			}
			schema.AddTypes(def)
			schema.AddPossibleType(def.Name, def)
		}
		for _, t := range s.scalars {
			def := definition(ast.Scalar, t, view)
			schema.AddTypes(def)
			schema.AddPossibleType(def.Name, def)
		}
		for _, t := range s.typeDefs {
			def := t.TypeDefinition(view)
			schema.AddTypes(def)
			schema.AddPossibleType(def.Name, def)
		}
		schema.Directives = map[string]*ast.DirectiveDefinition{}
		for n, d := range s.directives {
			schema.Directives[n] = d.DirectiveDefinition(view)
		}
		h := xxh3.New()
		json.NewEncoder(h).Encode(schema)
		s.schemas[view] = schema
		s.schemaDigests[view] = digest.NewDigest(XXH3, h)
	})

	return s.schemas[view]
}

// SchemaDigest returns the digest of the current schema.
func (s *Server) SchemaDigest() digest.Digest {
	s.Schema() // ensure it's built
	s.schemaLock.Lock()
	defer s.schemaLock.Unlock()
	return s.schemaDigests[s.View]
}

// Complexity returns the complexity of the given field.
func (s *Server) Complexity(ctx context.Context, typeName, field string, childComplexity int, args map[string]any) (int, bool) {
	// TODO
	return 1, false
}

// ExtendedError is an error that can provide extra data in an error response.
type ExtendedError interface {
	error
	Extensions() map[string]any
}

// Exec implements graphql.ExecutableSchema.
func (s *Server) Exec(ctx1 context.Context) graphql.ResponseHandler {
	return func(ctx context.Context) (res *graphql.Response) {
		gqlOp := graphql.GetOperationContext(ctx)

		if err := gqlOp.Validate(ctx); err != nil {
			return graphql.ErrorResponse(ctx, "validate: %s", err)
		}

		results, err := s.ExecOp(ctx, gqlOp)
		if err != nil {
			return &graphql.Response{
				Errors: gqlErrs(err),
			}
		}

		data, err := json.Marshal(results)
		if err != nil {
			return graphql.ErrorResponse(ctx, "marshal: %s", err)
		}

		return &graphql.Response{
			Data: json.RawMessage(data),
		}
	}
}

func gqlErrs(err error) (errs gqlerror.List) {
	if list, ok := err.(gqlerror.List); ok {
		return list
	}
	if unwrap, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range unwrap.Unwrap() {
			errs = append(errs, gqlErrs(e)...)
		}
	} else if err != nil {
		errs = append(errs, gqlErr(err, nil))
	}
	return
}

func (s *Server) ExecOp(ctx context.Context, gqlOp *graphql.OperationContext) (map[string]any, error) {
	if gqlOp.Doc == nil {
		var err error
		gqlOp.Doc, err = parser.ParseQuery(&ast.Source{Input: gqlOp.RawQuery})
		if err != nil {
			return nil, gqlErrs(err)
		}

		listErr := validator.Validate(s.Schema(), gqlOp.Doc)
		if len(listErr) != 0 {
			for _, e := range listErr {
				errcode.Set(e, errcode.ValidationFailed)
			}
			return nil, listErr
		}
	}
	results := make(map[string]any)
	for _, op := range gqlOp.Doc.Operations {
		switch op.Operation {
		case ast.Query:
			if gqlOp.OperationName != "" && gqlOp.OperationName != op.Name {
				continue
			}
			sels, err := s.parseASTSelections(ctx, gqlOp, s.root.Type(), op.SelectionSet)
			if err != nil {
				return nil, fmt.Errorf("query:\n%s\n\nerror: parse selections: %w", gqlOp.RawQuery, err)
			}
			results, err = s.Resolve(ctx, s.root, sels...)
			if err != nil {
				return nil, err
			}
		case ast.Mutation:
			// TODO
			return nil, fmt.Errorf("mutations not supported")
		case ast.Subscription:
			// TODO
			return nil, fmt.Errorf("subscriptions not supported")
		}
	}
	return results, nil
}

// Resolve resolves the given selections on the given object.
//
// Each selection is resolved in parallel, and the results are returned in a
// map whose keys correspond to the selection's field name or alias.
func (s *Server) Resolve(ctx context.Context, self Object, sels ...Selection) (map[string]any, error) {
	results := new(sync.Map)

	pool := pool.New().WithErrors()
	for _, sel := range sels {
		pool.Go(func() error {
			res, err := s.resolvePath(ctx, self, sel)
			if err != nil {
				return err
			}
			results.Store(sel.Name(), res)
			return nil
		})
	}
	if err := pool.Wait(); err != nil {
		return nil, gqlErrs(err)
	}

	resultsMap := make(map[string]any)
	results.Range(func(key, value any) bool {
		resultsMap[key.(string)] = value
		return true
	})
	return resultsMap, nil
}

// Load loads the object with the given ID.
func (s *Server) Load(ctx context.Context, id *call.ID) (Object, error) {
	res, id, err := s.loadType(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	return s.toSelectable(id, res)
}

func (s *Server) LoadType(ctx context.Context, id *call.ID) (Typed, error) {
	res, _, err := s.loadType(ctx, id)
	return res, err
}

func (s *Server) loadType(ctx context.Context, id *call.ID) (Typed, *call.ID, error) {
	var base Object
	var err error
	if id.Receiver() != nil {
		nth := int(id.Nth())
		if nth == 0 {
			base, err = s.Load(ctx, id.Receiver())
			if err != nil {
				return nil, nil, fmt.Errorf("load base: %w", err)
			}
		} else {
			// we are selecting the nth element of an enumerable, load the list
			// we are selecting from and then select the nth element from it rather
			// than trying to call the field on the object
			baseTyped, err := s.LoadType(ctx, id.Receiver())
			if err != nil {
				return nil, nil, fmt.Errorf("load base enum: %w", err)
			}
			baseEnum, isEnum := UnwrapAs[Enumerable](baseTyped)
			if !isEnum {
				return nil, nil, fmt.Errorf("cannot select item %d from non-enumerable %T", nth, baseTyped)
			}
			res, err := baseEnum.Nth(nth)
			if err != nil {
				return nil, nil, fmt.Errorf("nth %d: %w", nth, err)
			}
			if wrapped, ok := res.(Derefable); ok {
				res, ok = wrapped.Deref()
				if !ok {
					// the nth element is nil, maybe this should be allowed but for now error out
					return nil, nil, fmt.Errorf("item %d is null from enumerable", nth)
				}
			}
			return res, id, nil
		}
	} else {
		base = s.root
	}

	res, id, err := base.Call(ctx, s, id)
	return res, id, err
}

// Select evaluates a series of chained field selections starting from the
// given object and assigns the final result value into dest.
func (s *Server) Select(ctx context.Context, self Object, dest any, sels ...Selector) error {
	// Annotate ctx with the internal flag so we can distinguish self-calls from
	// user-calls in the UI.
	ctx = withInternal(ctx)

	var res Typed = self
	var id *call.ID
	for i, sel := range sels {
		var err error
		res, id, err = self.Select(ctx, s, sel)
		if err != nil {
			return fmt.Errorf("select: %w", err)
		}

		if res == nil {
			// null result; nothing to do
			return nil
		}

		destV := reflect.ValueOf(dest).Elem()
		if res.Type().Elem != nil {
			if i+1 < len(sels) {
				return fmt.Errorf("cannot sub-select enum of %s", res.Type())
			}
			if destV.Type().Kind() != reflect.Slice {
				// assigning to something like dagql.Typed, don't need to enumerate
				break
			}
			isObj := s.isObjectType(res.Type().Elem.Name())
			enum, isEnum := res.(Enumerable)
			if !isEnum {
				return fmt.Errorf("cannot assign non-Enumerable %T to %s", res, destV.Type())
			}
			// HACK: list of objects must be the last selection right now unless nth used in Selector.
			if i+1 < len(sels) {
				return fmt.Errorf("cannot sub-select enum of %s", res.Type())
			}
			for nth := 1; nth <= enum.Len(); nth++ {
				val, err := enum.Nth(nth)
				if err != nil {
					return fmt.Errorf("nth %d: %w", nth, err)
				}
				if wrapped, ok := val.(Derefable); ok {
					val, ok = wrapped.Deref()
					if !ok {
						if err := appendAssign(destV, nil); err != nil {
							return err
						}
						continue
					}
				}
				if isObj {
					nthID := id.SelectNth(nth)
					val, err = s.toSelectable(nthID, val)
					if err != nil {
						return fmt.Errorf("select %dth array element: %w", nth, err)
					}
				}
				if err := appendAssign(destV, val); err != nil {
					return err
				}
			}
			return nil
		} else if s.isObjectType(res.Type().Name()) {
			// if the result is an Object, set it as the next selection target, and
			// assign res to the "hydrated" Object
			self, err = s.toSelectable(id, res)
			if err != nil {
				return err
			}
			res = self
		} else if i+1 < len(sels) {
			// if the result is not an object and there are further selections,
			// that's a logic error.
			return fmt.Errorf("cannot sub-select %s", res.Type())
		}
	}
	return assign(reflect.ValueOf(dest).Elem(), res)
}

func (s *Server) isObjectType(typeName string) bool {
	_, ok := s.ObjectType(typeName)
	return ok
}

// Attach an install hook
func (s *Server) AddInstallHook(hook InstallHook) {
	s.installLock.Lock()
	s.installHooks = append(s.installHooks, hook)
	s.installLock.Unlock()
}

func (s *Server) SelectID(ctx context.Context, self Object, sels ...Selector) (*call.ID, error) {
	var res Typed = self
	var id *call.ID
	for i, sel := range sels {
		var err error
		res, id, err = self.ReturnType(ctx, s, sel)
		if err != nil {
			return nil, fmt.Errorf("select: %w", err)
		}

		if _, ok := s.ObjectType(res.Type().Name()); ok {
			// if the result is an Object, set it as the next selection target, and
			// assign res to the "hydrated" Object
			self, err = s.toSelectable(id, res)
			if err != nil {
				return nil, err
			}
			res = self
		} else if i+1 < len(sels) {
			// if the result is not an object and there are further selections,
			// that's a logic error.
			return nil, fmt.Errorf("cannot sub-select %s", res.Type())
		}
	}
	return id, nil
}

func LoadIDs[T Typed](ctx context.Context, srv *Server, ids []ID[T]) ([]T, error) {
	out := make([]T, len(ids))
	eg := new(errgroup.Group)
	for i, id := range ids {
		eg.Go(func() error {
			val, err := id.Load(ctx, srv)
			if err != nil {
				return err
			}
			out[i] = val.Self
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return out, err
	}
	return out, nil
}

func LoadIDInstances[T Typed](ctx context.Context, srv *Server, ids []ID[T]) ([]Instance[T], error) {
	out := make([]Instance[T], len(ids))
	eg := new(errgroup.Group)
	for i, id := range ids {
		eg.Go(func() error {
			val, err := id.Load(ctx, srv)
			if err != nil {
				return err
			}
			out[i] = val
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return out, err
	}
	return out, nil
}

type idCtx struct{}

func idToContext(ctx context.Context, id *call.ID) context.Context {
	return context.WithValue(ctx, idCtx{}, id)
}

func ContextWithID(ctx context.Context, id *call.ID) context.Context {
	return idToContext(ctx, id)
}

func CurrentID(ctx context.Context) *call.ID {
	val := ctx.Value(idCtx{})
	if val == nil {
		return nil
	}
	return val.(*call.ID)
}

type srvCtx struct{}

func srvToContext(ctx context.Context, srv *Server) context.Context {
	return context.WithValue(ctx, srvCtx{}, srv)
}

func CurrentDagqlServer(ctx context.Context) *Server {
	val := ctx.Value(srvCtx{})
	if val == nil {
		return nil
	}
	return val.(*Server)
}

// NewInstanceForCurrentID creates a new Instance that's set to the current ID from
// the given self value.
func NewInstanceForCurrentID[P, T Typed](
	ctx context.Context,
	srv *Server,
	parent Instance[P],
	self T,
) (Instance[T], error) {
	return NewInstanceForID(srv, parent, self, CurrentID(ctx))
}

// NewInstanceForID creates a new Instance with the given ID and self value.
func NewInstanceForID[P, T Typed](
	srv *Server,
	parent Instance[P],
	self T,
	id *call.ID,
) (Instance[T], error) {
	if id == nil {
		return Instance[T]{}, errors.New("id is nil")
	}

	objType, ok := srv.ObjectType(self.Type().Name())
	if !ok {
		return Instance[T]{}, fmt.Errorf("unknown type %q", self.Type().Name())
	}
	class, ok := objType.(Class[T])
	if !ok {
		return Instance[T]{}, fmt.Errorf("not a Class: %T", objType)
	}

	// sanity check: verify the ID matches the type of self
	if id.Type().NamedType() != objType.TypeName() {
		return Instance[T]{}, fmt.Errorf("expected ID of type %q, got %q", objType.TypeName(), id.Type().NamedType())
	}

	return Instance[T]{
		Constructor: id,
		Self:        self,
		Class:       class,
		Module:      parent.Module,
	}, nil
}

func idToPath(id *call.ID) ast.Path {
	path := ast.Path{}
	if id == nil { // Query
		return path
	}
	if id.Receiver() != nil {
		path = idToPath(id.Receiver())
	}
	path = append(path, ast.PathName(id.Field()))
	if id.Nth() != 0 {
		path = append(path, ast.PathIndex(id.Nth()-1))
	}
	return path
}

func gqlErr(rerr error, path ast.Path) *gqlerror.Error {
	var gqlErr *gqlerror.Error
	if errors.As(rerr, &gqlErr) {
		if len(gqlErr.Path) == 0 {
			gqlErr.Path = path
		}
		return gqlErr
	}
	gqlErr = &gqlerror.Error{
		Err:     rerr,
		Message: rerr.Error(),
		Path:    path,
	}
	var ext ExtendedError
	if errors.As(rerr, &ext) {
		gqlErr.Extensions = ext.Extensions()
	}
	return gqlErr
}

func (s *Server) resolvePath(ctx context.Context, self Object, sel Selection) (res any, rerr error) {
	defer func() {
		if r := recover(); r != nil {
			rerr = PanicError{
				Cause:     r,
				Self:      self,
				Selection: sel,
				Stack:     debug.Stack(),
			}
		}

		if rerr != nil {
			rerr = gqlErr(rerr, append(idToPath(self.ID()), ast.PathName(sel.Name())))
		}
	}()

	if sel.Selector.Nth != 0 {
		// NOTE: this is explicitly not handled - but it's fine because
		// resolvePath is called from selectors from field parsing, so we
		// shouldn't hit this in practice
		return nil, fmt.Errorf("cannot resolve selector path with nth")
	}

	val, chainedID, err := self.Select(ctx, s, sel.Selector)
	if err != nil {
		return nil, err
	}

	if val == nil {
		// a nil value ignores all sub-selections
		return nil, nil
	}

	enum, ok := val.(Enumerable)
	if ok {
		// we're sub-selecting into an enumerable value, so we need to resolve each
		// element

		// TODO arrays of arrays
		results := []any{} // TODO subtle: favor [] over null result
		for nth := 1; nth <= enum.Len(); nth++ {
			val, err := enum.Nth(nth)
			if err != nil {
				return nil, err
			}
			if wrapped, ok := val.(Derefable); ok {
				val, ok = wrapped.Deref()
				if !ok {
					results = append(results, nil)
					continue
				}
			}
			if len(sel.Subselections) == 0 {
				results = append(results, val)
			} else {
				nthID := chainedID.SelectNth(nth)
				node, err := s.toSelectable(nthID, val)
				if err != nil {
					return nil, fmt.Errorf("instantiate %dth array element: %w", nth, err)
				}
				res, err := s.Resolve(ctx, node, sel.Subselections...)
				if err != nil {
					return nil, err
				}
				results = append(results, res)
			}
		}
		return results, nil
	}

	if len(sel.Subselections) == 0 {
		return val, nil
	}

	// instantiate the return value so we can sub-select
	node, err := s.toSelectable(chainedID, val)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}

	return s.Resolve(ctx, node, sel.Subselections...)
}

func (s *Server) toSelectable(chainedID *call.ID, val Typed) (Object, error) {
	if sel, ok := val.(Object); ok {
		// We always support returning something that's already Selectable, e.g. an
		// object loaded from its ID.
		return sel, nil
	}
	class, ok := s.ObjectType(val.Type().Name())
	if !ok {
		return nil, fmt.Errorf("toSelectable: unknown type %q", val.Type().Name())
	}
	return class.New(chainedID, val)
}

func (s *Server) parseASTSelections(ctx context.Context, gqlOp *graphql.OperationContext, self *ast.Type, astSels ast.SelectionSet) ([]Selection, error) {
	vars := gqlOp.Variables

	class := s.objects[self.Name()]
	if class == nil {
		return nil, fmt.Errorf("parseASTSelections: not an Object type: %q", self.Name())
	}

	sels := []Selection{}
	for _, sel := range astSels {
		switch x := sel.(type) {
		case *ast.Field:
			sel, resType, err := class.ParseField(ctx, s.View, x, vars)
			if err != nil {
				return nil, fmt.Errorf("parse field %q: %w", x.Name, err)
			}
			var subsels []Selection
			if len(x.SelectionSet) > 0 {
				subsels, err = s.parseASTSelections(ctx, gqlOp, resType, x.SelectionSet)
				if err != nil {
					return nil, err
				}
			}

			sels = append(sels, Selection{
				Alias:         x.Alias,
				Selector:      sel,
				Subselections: subsels,
			})
		case *ast.FragmentSpread:
			fragment := gqlOp.Doc.Fragments.ForName(x.Name)
			if fragment == nil {
				return nil, fmt.Errorf("unknown fragment: %s", x.Name)
			}
			if len(fragment.SelectionSet) > 0 {
				subsels, err := s.parseASTSelections(ctx, gqlOp, self, fragment.SelectionSet)
				if err != nil {
					return nil, err
				}
				sels = append(sels, subsels...)
			}
		default:
			return nil, fmt.Errorf("unknown field type: %T", x)
		}
	}

	return sels, nil
}

// Selection represents a selection of a field on an object.
type Selection struct {
	Alias         string
	Selector      Selector
	Subselections []Selection
}

// Name returns the name of the selection, which is either the alias or the
// field name.
func (sel Selection) Name() string {
	if sel.Alias != "" {
		return sel.Alias
	}
	return sel.Selector.Field
}

// Selector specifies how to retrieve a value from an Instance.
type Selector struct {
	Field string
	Args  []NamedInput
	Nth   int
	View  View
}

func (sel Selector) String() string {
	str := sel.Field
	if len(sel.Args) > 0 {
		str += "("
		for i, arg := range sel.Args {
			if i > 0 {
				str += ", "
			}
			str += arg.String()
		}
		str += ")"
	}
	if sel.Nth != 0 {
		str += fmt.Sprintf("[%d]", sel.Nth)
	}
	return str
}

type Inputs []NamedInput

func (args Inputs) Lookup(name string) (Input, bool) {
	for _, arg := range args {
		if arg.Name == name {
			return arg.Value, true
		}
	}
	return nil, false
}

type NamedInput struct {
	Name  string
	Value Input
}

func (arg NamedInput) String() string {
	return fmt.Sprintf("%s: %v", arg.Name, arg.Value.ToLiteral().ToAST())
}

type DecoderFunc func(any) (Input, error)

var _ InputDecoder = DecoderFunc(nil)

func (f DecoderFunc) DecodeInput(val any) (Input, error) {
	return f(val)
}

type InputObject[T Type] struct {
	Value T
}

var _ Input = InputObject[Type]{} // TODO

func (InputObject[T]) Type() *ast.Type {
	var zero T
	return &ast.Type{
		NamedType: zero.TypeName(),
		NonNull:   true,
	}
}

func (InputObject[T]) Decoder() InputDecoder {
	return DecoderFunc(func(val any) (Input, error) {
		vals, ok := val.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected map[string]any, got %T", val)
		}
		var obj T
		if err := setInputObjectFields(&obj, vals); err != nil {
			return nil, err
		}
		return InputObject[T]{
			Value: obj,
		}, nil
	})
}

func setInputObjectFields(obj any, vals map[string]any) error {
	objT := reflect.TypeOf(obj).Elem()
	objV := reflect.ValueOf(obj)
	if objT.Kind() != reflect.Struct {
		// TODO handle pointer?
		return fmt.Errorf("object must be a struct, got %T", obj)
	}
	for i := range objT.NumField() {
		fieldT := objT.Field(i)
		fieldV := objV.Elem().Field(i)
		name := fieldT.Tag.Get("name")
		if name == "" {
			name = strcase.ToLowerCamel(fieldT.Name)
		}
		if name == "-" {
			continue
		}
		fieldI := fieldV.Interface()
		if fieldT.Anonymous {
			// embedded struct
			val := reflect.New(fieldT.Type)
			if err := setInputObjectFields(val.Interface(), vals); err != nil {
				return err
			}
			fieldV.Set(val.Elem())
			continue
		}
		zeroInput, err := builtinOrInput(fieldI)
		if err != nil {
			return fmt.Errorf("arg %q: %w", fieldT.Name, err)
		}
		var input Input
		if val, ok := vals[name]; ok {
			var err error
			input, err = zeroInput.Decoder().DecodeInput(val)
			if err != nil {
				return err
			}
		} else if inputDefStr, hasDefault := fieldT.Tag.Lookup("default"); hasDefault {
			var err error
			input, err = zeroInput.Decoder().DecodeInput(inputDefStr)
			if err != nil {
				return fmt.Errorf("convert default value for arg %s: %w", name, err)
			}
		} else if zeroInput.Type().NonNull {
			return fmt.Errorf("missing required input field %q", name)
		}
		if input != nil { // will be nil for optional fields
			if err := assign(fieldV, input); err != nil {
				return fmt.Errorf("assign input object %q as %+v (%T): %w", fieldT.Name, input, input, err)
			}
		}
	}
	return nil
}

func (input InputObject[T]) ToLiteral() call.Literal {
	obj := input.Value
	args, err := collectLiteralArgs(obj)
	if err != nil {
		panic(fmt.Errorf("collectLiteralArgs: %w", err))
	}
	return call.NewLiteralObject(args...)
}

func collectLiteralArgs(obj any) ([]*call.Argument, error) {
	objT := reflect.TypeOf(obj)
	objV := reflect.ValueOf(obj)
	if objV.Kind() != reflect.Struct {
		// TODO handle pointer?
		return nil, fmt.Errorf("object must be a struct, got %T", obj)
	}
	args := []*call.Argument{}
	for i := range objV.NumField() {
		fieldT := objT.Field(i)
		name := fieldT.Tag.Get("name")
		if name == "" {
			name = strcase.ToLowerCamel(fieldT.Name)
		}
		if name == "-" {
			continue
		}
		fieldI := objV.Field(i).Interface()
		if fieldT.Anonymous {
			subArgs, err := collectLiteralArgs(fieldI)
			if err != nil {
				return nil, fmt.Errorf("arg %q: %w", fieldT.Name, err)
			}
			args = append(args, subArgs...)
			continue
		}
		input, err := builtinOrInput(fieldI)
		if err != nil {
			return nil, fmt.Errorf("arg %q: %w", fieldT.Name, err)
		}
		args = append(args, call.NewArgument(
			name,
			input.ToLiteral(),
			false,
		))
	}
	return args, nil
}
