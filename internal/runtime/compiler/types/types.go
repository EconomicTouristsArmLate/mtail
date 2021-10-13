// Copyright 2016 Google Inc. All Rights Reserved.
// This file is available under the Apache license.

package types

import (
	"errors"
	"fmt"
	"regexp/syntax"
	"strings"
	"sync"

	"github.com/golang/glog"
)

// Type represents a type in the mtail program.
type Type interface {
	// Root returns an exemplar Type after unification occurs.  If the type
	// system is complete after unification, Root will be a TypeOperator.
	Root() Type

	// String returns a string representation of a Type.
	String() string
}

// TypeError describes an error in which a type was expected, but another was encountered.
type TypeError struct {
	error    error
	expected Type
	received Type
}

var (
	ErrRecursiveUnification = errors.New("recursive unification error")
	ErrTypeMismatch         = errors.New("type mismatch")
	ErrInternal             = errors.New("internal error")
)

func (e *TypeError) Root() Type {
	return e
}

func (e *TypeError) String() string {
	if e == nil || e.error == nil {
		return fmt.Sprintf("type error")
	}
	var estr, rstr string
	if IsComplete(e.expected) {
		estr = e.expected.String()
	} else {
		estr = "incomplete type"
	}
	if IsComplete(e.received) {
		rstr = e.received.String()
	} else {
		rstr = "incomplete type"
	}
	glog.V(2).Infof("%s: expected %q received %q", e.error, e.expected, e.received)
	return fmt.Sprintf("%s; expected %s received %s", e.error, estr, rstr)
}

func (e TypeError) Error() string {
	return e.String()
}

func (e *TypeError) Unwrap() error {
	return e.error
}

// AsTypeError behaves like `errors.As`, attempting to cast the type `t` into a
// provided `target` TypeError and returning if it was successful.
func AsTypeError(t Type, target **TypeError) (ok bool) {
	*target, ok = t.(*TypeError)
	return ok
}

// IsTypeError behaves like `errors.Is`, indicating that the type is a TypeError.
func IsTypeError(t Type) bool {
	var e *TypeError
	return AsTypeError(t, &e)
}

var (
	nextVariableIDMu sync.Mutex
	nextVariableID   int
)

// Variable represents an unbound type variable in the type system.
type Variable struct {
	ID int

	instanceMu sync.RWMutex
	Instance   Type
}

// NewVariable constructs a new unique TypeVariable.
func NewVariable() *Variable {
	nextVariableIDMu.Lock()
	id := nextVariableID
	nextVariableID++
	nextVariableIDMu.Unlock()
	return &Variable{ID: id}
}

// Root returns an exemplar of this TypeVariable, in this case the root of the unification tree.
func (t *Variable) Root() Type {
	t.instanceMu.Lock()
	defer t.instanceMu.Unlock()
	if t.Instance == nil {
		return t
	}
	r := t.Instance.Root()
	t.Instance = r
	return r
}

func (t *Variable) String() string {
	t.instanceMu.RLock()
	defer t.instanceMu.RUnlock()
	if t.Instance != nil {
		return t.Instance.String()
	}
	return fmt.Sprintf("typeVar%d", t.ID)
}

// SetInstance sets the exemplar instance of this TypeVariable, during
// unification.
func (t *Variable) SetInstance(t1 Type) {
	t.instanceMu.Lock()
	defer t.instanceMu.Unlock()
	t.Instance = t1
}

// Operator represents a type scheme in the type system.
type Operator struct {
	// Name is a common name for this operator
	Name string
	// Args is the sequence of types that are parameters to this type.  They
	// may be fully bound type operators, or partially defined (i.e. contain
	// TypeVariables) in which case they represent polymorphism in the operator
	// they are arguments to.
	Args []Type
}

// Root returns an exemplar of a TypeOperator, i.e. itself.
func (t *Operator) Root() Type {
	return t
}

func (t *Operator) String() (s string) {
	switch l := len(t.Args); {
	case l < 2:
		s = t.Name
		for _, a := range t.Args {
			s += " " + a.String()
		}
	default:
		s = t.Args[0].String()
		for _, a := range t.Args[1:] {
			s += t.Name + a.String()
		}
	}
	return s
}

// Function is a convenience method, which instantiates a new Function type
// scheme, with the given args as parameters.
func Function(args ...Type) *Operator {
	return &Operator{"→", args}
}

// IsFunction returns true if the given type is a Function type.
func IsFunction(t Type) bool {
	if v, ok := t.(*Operator); ok {
		return v.Name == "→"
	}
	return false
}

// Dimension is a convenience method which instantiates a new Dimension type
// scheme, with the given args as the dimensions of the type.  (This type looks
// a lot like a Product type.)
func Dimension(args ...Type) *Operator {
	return &Operator{"⨯", args}
}

// IsDimension returns true if the given type is a Dimension type.
func IsDimension(t Type) bool {
	if v, ok := t.(*Operator); ok {
		return v.Name == "⨯"
	}
	return false
}

// IsComplete returns true if the type and all its arguments have non-variable exemplars.
func IsComplete(t Type) bool {
	switch v := t.Root().(type) {
	case *Variable:
		return false
	case *Operator:
		for _, a := range v.Args {
			if !IsComplete(a) {
				return false
			}
		}
		return true
	}
	return false
}

// Builtin type constants.
var (
	Error         = &TypeError{}
	InternalError = &TypeError{error: ErrInternal}
	Undef         = &Operator{"Undef", []Type{}}
	None          = &Operator{"None", []Type{}}
	Bool          = &Operator{"Bool", []Type{}}
	Int           = &Operator{"Int", []Type{}}
	Float         = &Operator{"Float", []Type{}}
	String        = &Operator{"String", []Type{}}
	Pattern       = &Operator{"Pattern", []Type{}}
	// TODO(jaq): use composite type so we can typecheck the bucket directly, e.g. hist[j] = i.
	Buckets = &Operator{"Buckets", []Type{}}
)

// Builtins is a mapping of the builtin language functions to their type definitions.
var Builtins = map[string]Type{
	"int":         Function(NewVariable(), Int),
	"bool":        Function(NewVariable(), Bool),
	"float":       Function(NewVariable(), Float),
	"string":      Function(NewVariable(), String),
	"timestamp":   Function(Int),
	"len":         Function(String, Int),
	"settime":     Function(Int, None),
	"strptime":    Function(String, String, None),
	"strtol":      Function(String, Int, Int),
	"tolower":     Function(String, String),
	"getfilename": Function(String),
	"subst":       Function(Pattern, String, String, String),
}

// FreshType returns a new type from the provided type scheme, replacing any
// unbound type variables with new type variables.
func FreshType(t Type) Type {
	// mappings keeps track of replaced variables in this type so that t -> t
	// becomes q -> q not q -> r
	mappings := make(map[*Variable]*Variable)

	var freshRec func(Type) Type
	freshRec = func(tp Type) Type {
		p := tp.Root()
		switch p1 := p.(type) {
		case *Variable:
			if _, ok := mappings[p1]; !ok {
				mappings[p1] = NewVariable()
			}
			return mappings[p1]
		case *Operator:
			args := make([]Type, 0, len(p1.Args))
			for _, arg := range p1.Args {
				args = append(args, freshRec(arg))
			}
			return &Operator{p1.Name, args}
		default:
			glog.V(1).Infof("Unexpected type p1: %v", p1)
		}
		return tp
	}
	return freshRec(t)
}

// occursIn returns true if `v` is in any of `types`.
func occursIn(v Type, types []Type) bool {
	for _, t2 := range types {
		if occursInType(v, t2) {
			return true
		}
	}
	return false
}

// occursInType returns true if `v` is `t2` or recursively contained within `t2`.
func occursInType(v Type, t2 Type) bool {
	root := t2.Root()
	if Equals(root, v) {
		return true
	}
	if to, ok := root.(*Operator); ok {
		return occursIn(v, to.Args)
	}
	return false
}

// Equals compares two types, testing for equality.
func Equals(t1, t2 Type) bool {
	t1, t2 = t1.Root(), t2.Root()
	switch t1 := t1.(type) {
	case *Variable:
		r2, ok := t2.(*Variable)
		if !ok {
			return occursInType(t1, t2)
		}
		return t1.ID == r2.ID
	case *Operator:
		t2, ok := t2.(*Operator)
		if !ok {
			return false
		}
		if t1.Name != t2.Name {
			return false
		}
		if len(t1.Args) != len(t2.Args) {
			return false
		}
		for i := range t1.Args {
			if !Equals(t1.Args[i], t2.Args[2]) {
				return false
			}
		}
		return true
	case *TypeError:
		return false
	}
	return true
}

// Unify performs type unification of both parameter Types.  It returns the
// least upper bound of both types, the smallest type that is capable of
// representing both parameters.  If either type is a type variable, then that
// variable is unified with the LUB.  In reporting errors, it is assumed that a
// is the expected type and b is the type observed.
func Unify(a, b Type) Type {
	glog.V(2).Infof("Unifying %v and %v", a, b)
	a1, b1 := a.Root(), b.Root()
	switch a2 := a1.(type) {
	case *Variable:
		switch b2 := b1.(type) {
		case *Variable:
			if a2.ID != b2.ID {
				glog.V(2).Infof("Making %q type %q", a2, b1)
				a2.SetInstance(b1)
				return b1
			}
			return a1
		case *Operator:
			if occursInType(a2, b2) {
				return &TypeError{ErrRecursiveUnification, a2, b2}
			}
			glog.V(2).Infof("Making %q type %q", a2, b1)
			a2.SetInstance(b1)
			return b1
		}
	case *Operator:
		switch b2 := b1.(type) {
		case *Variable:
			t := Unify(b, a)
			var e *TypeError
			if AsTypeError(t, &e) {
				return &TypeError{ErrTypeMismatch, e.received, e.expected}
			}
			return t

		case *Operator:
			if len(a2.Args) != len(b2.Args) {
				return &TypeError{ErrTypeMismatch, a2, b2}
			}
			var rType *Operator
			if a2.Name != b2.Name {
				t := LeastUpperBound(a, b)
				glog.V(2).Infof("Got LUB = %#v", t)
				var e *TypeError
				if AsTypeError(t, &e) {
					return e
				}
				var ok bool
				if rType, ok = t.(*Operator); !ok {
					return &TypeError{ErrInternal, a2, b2}
				}
			} else {
				rType = &Operator{a2.Name, []Type{}}
			}
			rType.Args = make([]Type, len(a2.Args))
			for i, argA := range a2.Args {
				t := Unify(argA, b2.Args[i])
				var e *TypeError
				if AsTypeError(t, &e) {
					return e
				}
				rType.Args[i] = t
			}
			return rType
		}
	}
	return &TypeError{ErrInternal, a, b}
}

// LeastUpperBound returns the smallest type that may contain both parameter types.
func LeastUpperBound(a, b Type) Type {
	a1, b1 := a.Root(), b.Root()
	glog.V(2).Infof("Computing LUB(%q, %q)", a1, b1)

	if Equals(a1, b1) {
		return a1
	}
	// If either is a TypeVariable, the other is the lub
	if _, ok := a1.(*Variable); ok {
		return b1
	}
	if _, ok := b1.(*Variable); ok {
		return a1
	}
	// If either is Undef, other is the lub
	if Equals(a1, Undef) {
		return b1
	}
	if Equals(b1, Undef) {
		return a1
	}
	// Easy substitutions
	if (Equals(a1, Float) && Equals(b1, Int)) ||
		(Equals(b1, Float) && Equals(a1, Int)) {
		return Float
	}
	if (Equals(a1, String) && Equals(b1, Int)) ||
		(Equals(b1, String) && Equals(a1, Int)) ||
		(Equals(a1, String) && Equals(b1, Float)) ||
		(Equals(b1, String) && Equals(a1, Float)) {
		return String
	}
	if (Equals(a1, Pattern) && Equals(b1, Bool)) ||
		(Equals(a1, Bool) && Equals(b1, Pattern)) {
		return Bool
	}
	if (Equals(a1, Bool) && Equals(b1, Int)) ||
		(Equals(a1, Int) && Equals(b1, Bool)) {
		return Int
	}
	// A string can be a pattern, but not vice versa.
	if (Equals(a1, String) && Equals(b1, Pattern)) ||
		(Equals(a1, Pattern) && Equals(b1, String)) {
		return Pattern
	}
	// A pattern and an Int are Bool
	if (Equals(a1, Pattern) && Equals(b1, Int)) ||
		(Equals(a1, Int) && Equals(b1, Pattern)) {
		return Bool
	}
	return &TypeError{ErrTypeMismatch, a, b}
}

// inferCaprefType determines a type for the nth capturing group in re, based on contents
// of that capture group.
func InferCaprefType(re *syntax.Regexp, n int) Type {
	group := getCaptureGroup(re, n)
	if group == nil {
		return None
	}

	if group.Op != syntax.OpAlternate {
		return inferGroupType(group)
	}

	subType := Type(Undef)
	for _, sub := range group.Sub {
		subType = LeastUpperBound(subType, inferGroupType(sub))
	}
	return subType
}

func inferGroupType(group *syntax.Regexp) Type {
	switch {
	case groupOnlyMatches(group, "+-"):
		return String
	case groupOnlyMatches(group, "+-0123456789"):
		// Must be at least one digit in the group.
		if !strings.ContainsAny(group.String(), "0123456789") {
			return String
		}
		if group.Op == syntax.OpAlternate || group.Op == syntax.OpCharClass {
			return String
		}
		return Int
	case groupOnlyMatches(group, "+-0123456789.eE"):
		// Only one decimal point allowed.
		if strings.Count(group.String(), ".") > 1 {
			return String
		}
		return Float
	}
	return String
}

// getCaptureGroup returns the Regexp node of the capturing group numbered cgID
// in re.
func getCaptureGroup(re *syntax.Regexp, cgID int) *syntax.Regexp {
	if re.Op == syntax.OpCapture && re.Cap == cgID {
		return re.Sub[0]
	}
	for _, sub := range re.Sub {
		r := getCaptureGroup(sub, cgID)
		if r != nil {
			return r
		}
	}
	return nil
}

// groupOnlyMatches returns true iff group only matches runes in s.
func groupOnlyMatches(group *syntax.Regexp, s string) bool {
	switch group.Op {
	case syntax.OpLiteral:
		for _, r := range group.Rune {
			if !strings.ContainsRune(s, r) {
				return false
			}
		}
		return true

	case syntax.OpCharClass:
		for i := 0; i < len(group.Rune); i += 2 {
			lo, hi := group.Rune[i], group.Rune[i+1]
			for r := lo; r <= hi; r++ {
				if !strings.ContainsRune(s, r) {
					return false
				}
			}
		}
		return true

	case syntax.OpStar, syntax.OpPlus, syntax.OpRepeat, syntax.OpQuest, syntax.OpCapture:
		return groupOnlyMatches(group.Sub[0], s)

	case syntax.OpConcat, syntax.OpAlternate:
		for _, sub := range group.Sub {
			if !groupOnlyMatches(sub, s) {
				return false
			}
		}

	default:
		return false
	}
	return true
}
