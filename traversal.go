/*
 *    Copyright 2023 Stephen Guo
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 *
 */

package dfpt

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

type Traveller struct {
	adapter         reflect.Value
	conf            *TraverseConf
	prefixes        ItemTypes                      // group bindings run before all individually bindings
	suffixes        ItemTypes                      // group bindings run after all individually bindings
	shortcuts       map[ItemType]reflect.Value     // group bindings(ForNilPtr/ForIntX/ForUintX/ForAllKinds) -> binding methods
	typeMethods     map[reflect.Type]reflect.Value // type -> method
	kindMethods     map[reflect.Kind]reflect.Value // kind -> method
	typeOrder       orderItems                     // all type list in order (tag order or declare order)
	structTypeCache sync.Map
}

func NewTraveller(adapter interface{}, config ...*TraverseConf) (*Traveller, error) {
	aptVal := reflect.ValueOf(adapter)
	if !aptVal.IsValid() {
		return nil, ErrInvalidAdapter
	}
	aptType := aptVal.Type()
	var items orderItems
	shortcuts := make(map[ItemType]reflect.Value)
	typeMethods := make(map[reflect.Type]reflect.Value)
	kindMethods := make(map[reflect.Kind]reflect.Value)
	for i := 0; i < aptType.NumMethod(); i++ {
		m := aptType.Method(i)
		itype, inKind, ok := Unknown.Which(m.Name)
		if !ok {
			continue
		}
		if !itype.IsValidWithReceiver(m) {
			continue
		}
		fType := m.Func.Type()
		switch itype {
		case ForImpl, ForAssign:
			inType := fType.In(itype.ParamLength())
			if _, exist := typeMethods[inType]; exist {
				return nil, fmt.Errorf("duplicated binding function %s found for Type:%s", m.Name, inType.Name())
			}
			items = append(items, orderItem{
				i: i,
				n: m.Name,
				o: 0,
				t: inType,
				c: false, // there's no possibility of further in-depth analysis with explicit type binding
				k: reflect.Invalid,
			})
			typeMethods[inType] = aptVal.Method(i)
		case ForKind, ForContainer:
			if _, exist := kindMethods[inKind]; exist {
				return nil, fmt.Errorf("duplicated binding function %s found for Kind:%s", m.Name, inKind.String())
			}
			items = append(items, orderItem{
				i: i,
				n: m.Name,
				o: 0,
				t: nil,
				c: itype == ForContainer,
				k: inKind,
			})
			kindMethods[inKind] = aptVal.Method(i)
		case ForNilPtr, ForIntX, ForUintX, ForAllKinds:
			if _, exist := shortcuts[itype]; exist {
				return nil, fmt.Errorf("duplicated binding function %s found", m.Name)
			}
			shortcuts[itype] = aptVal.Method(i)
		}
	}
	if len(items) == 0 && len(shortcuts) == 0 {
		return nil, errors.New("no available binding function found")
	}
	sort.Sort(items)
	var conf *TraverseConf
	if len(config) > 0 && config[0] != nil {
		conf = config[0].Clone()
	}
	var prefixs, suffixs ItemTypes
	if len(shortcuts) > 0 {
		for k := range shortcuts {
			if k.Prefix() {
				prefixs = append(prefixs, k)
			} else if k.Suffix() {
				suffixs = append(suffixs, k)
			}
		}
		sort.Sort(prefixs)
		sort.Sort(suffixs)
	}
	return &Traveller{
		adapter:     aptVal,
		conf:        conf,
		prefixes:    prefixs,
		suffixes:    suffixs,
		shortcuts:   shortcuts,
		typeMethods: typeMethods,
		kindMethods: kindMethods,
		typeOrder:   items,
	}, nil
}

func (t *Traveller) String() string {
	if t == nil {
		return "Traveller<nil>"
	}
	adapterStr := ""
	if !t.adapter.IsValid() {
		adapterStr = "adapter:Invalid"
	} else {
		typ := t.adapter.Type()
		adapterStr = fmt.Sprintf("adapter:%s", typ.Name())
	}
	return fmt.Sprintf("Traveller{%s Prefixs:%s Suffixs:%s Types:%d Kinds:%d Items:%s}",
		adapterStr, t.prefixes, t.suffixes, len(t.typeMethods), len(t.kindMethods), []orderItem(t.typeOrder))
}

func (t *Traveller) _call(ctx *TravContext, parent *parentInfo, val reflect.Value) (goin, reEnter bool,
	info *parentInfo, newVal reflect.Value, err error) {
	if !val.IsValid() {
		return false, false, nil, reflect.Value{}, errors.New("invalid value")
	}

	// prefix shortcuts
	for _, itype := range t.prefixes {
		if itype.MatchValue(val) {
			method := t.shortcuts[itype]
			outs := method.Call(parent.callIns(ctx, val))
			_, err = itype.parseReturns(outs)
			return false, false, nil, reflect.Value{}, err
		}
	}

	for i, item := range t.typeOrder {
		itype, typ, kind, match := item.match(val)
		if !match {
			continue
		}
		var outs []reflect.Value
		if typ != nil {
			fVal, ok := t.typeMethods[typ]
			if !ok || !fVal.IsValid() {
				panic(fmt.Errorf("matching %d item %s, but function not found by Type:%s", i, item, typ.Name()))
			}
			outs = fVal.Call(parent.callIns(ctx, val))
		} else if kind != reflect.Invalid {
			fVal, ok := t.kindMethods[kind]
			if !ok || !fVal.IsValid() {
				panic(fmt.Errorf("matching %d item %s, but function not found by Kind:%s", i, item, kind.String()))
			}
			if _, isContainer := _containers[kind]; isContainer {
				var size int
				var fields []Property
				switch kind {
				case reflect.Array:
					size = val.Len()
				case reflect.Slice:
					if !val.IsNil() {
						size = val.Len()
					}
				case reflect.Map:
					if !val.IsNil() {
						size = val.Len() << 1
					}
				case reflect.Struct:
					size, fields = t._structProperties(val)
				case reflect.Ptr:
					if !val.IsNil() {
						size = 1
					}
				}
				info = &parentInfo{
					depth:        parent.nextDepth(),
					value:        val,
					size:         size,
					offset:       -1,
					structFields: fields,
					binding:      fVal,
				}
				outs = fVal.Call(parent.startContainerIns(ctx, info, val))
			} else {
				outs = fVal.Call(parent.callIns(ctx, val))
			}
		} else {
			panic(fmt.Errorf("SHOULD NOT BE HERE!! matching %d item %s, Kind:%s", i, item, kind.String()))
		}
		goin, err = itype.parseReturns(outs)
		if err != nil {
			return false, false, nil, reflect.Value{}, err
		}
		return goin, false, info, reflect.Value{}, nil
	}
	// no callback for specific value type
	if t.conf != nil && t.conf.PtrAutoGoIn {
		// no callback for Ptr
		if val.Type().Kind() == reflect.Ptr {
			if val.IsNil() == false {
				newVal = val.Elem()
				return false, true, parent, newVal, nil
			} else {
				return false, false, parent, reflect.Value{}, nil
			}
		}
	}
	// suffix shortcuts
	for _, itype := range t.suffixes {
		if itype.MatchValue(val) {
			method := t.shortcuts[itype]
			outs := method.Call(parent.callIns(ctx, val))
			_, err = itype.parseReturns(outs)
			return false, false, nil, reflect.Value{}, err
		}
	}
	// emit error if there's no flag for ignoring
	if t.conf == nil || !t.conf.IgnoreMissedBinding {
		return false, false, nil, reflect.Value{},
			fmt.Errorf("type:%s kind:%s binding is missing", val.Type(), val.Type().Kind())
	}
	return false, false, nil, reflect.Value{}, nil
}

func (t *Traveller) _structProperties(val reflect.Value) (int, []Property) {
	if !val.IsValid() {
		return 0, nil
	}
	if t.conf != nil && t.conf.Propertier != nil {
		return t.conf.Propertier.Properties(val)
	}
	var ps []Property
	typ := val.Type()
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.PkgPath == "" {
			ps = append(ps, Property{
				Index:        i,
				Name:         f.Name,
				IndexForReal: -1,
			})
		}
	}
	return len(ps), ps
}

func (t *Traveller) _traverse(ctx *TravContext, parent *parentInfo, val reflect.Value) error {
	if !val.IsValid() {
		return fmt.Errorf("invalid value in _traverse(parent:%s, val:%s)", parent, val.String())
	}
	var next *parentInfo
	var goin, reEnter bool
	var err error
	oldVal := val
	var newVal reflect.Value
	for {
		goin, reEnter, next, newVal, err = t._call(ctx, parent, oldVal)
		if err != nil {
			return err
		}
		if reEnter {
			if !newVal.IsValid() {
				panic(fmt.Errorf("reenter need a valid value, oldVal:%s", oldVal))
			}
			oldVal = newVal
			continue
		}
		if !goin {
			return nil
		}
		if next == nil {
			panic(fmt.Errorf("container value need next *parentInfo, parent:%s val:%s", parent, oldVal.String()))
		}
		break
	}
	switch oldVal.Kind() {
	case reflect.Array, reflect.Slice:
		for i := 0; i < next.size; i++ {
			child := oldVal.Index(i)
			next.offset = i
			if err = t._traverse(ctx, next, child); err != nil {
				return err
			}
		}
	case reflect.Map:
		if next.size > 0 {
			keys := oldVal.MapKeys()
			if len(keys)<<1 != next.size {
				panic(fmt.Errorf("next:%s but len(keys)==%d", next, len(keys)))
			}
			for i := 0; i < len(keys); i++ {
				// stack value for map: idx%2==0 is the key of map, idx%2==1 is the value of map
				next.offset = i << 1
				if err = t._traverse(ctx, next, keys[i]); err != nil {
					return err
				}
				value := oldVal.MapIndex(keys[i])
				next.offset = i<<1 + 1
				if err = t._traverse(ctx, next, value); err != nil {
					return err
				}
			}
		}
	case reflect.Struct:
		for i := 0; i < len(next.structFields); i++ {
			field := next.structFields[i]
			if field.Index < 0 {
				continue
			}
			fieldVal := oldVal.Field(field.Index)
			next.offset = i
			if err = t._traverse(ctx, next, fieldVal); err != nil {
				return err
			}
		}
	case reflect.Ptr:
		if next.size > 0 {
			elem := oldVal.Elem()
			next.offset = 0
			if err = t._traverse(ctx, next, elem); err != nil {
				return err
			}
		}
	default:
		panic("unknown status")
	}
	if t.conf != nil && t.conf.ContainerEnd {
		outs := next.binding.Call(parent.endContainerIns(ctx, next, oldVal))
		_, err = ForContainer.parseReturns(outs)
		if err != nil {
			return fmt.Errorf("call container end failed: %v", err)
		}
	}
	return nil
}

func (t *Traveller) Traverse(ctx *TravContext, obj interface{}) error {
	val := reflect.ValueOf(obj)
	if !val.IsValid() {
		return nil
	}
	return t._traverse(ctx, nil, val)
}
