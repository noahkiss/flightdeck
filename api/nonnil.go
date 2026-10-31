package api

import (
	"reflect"
	"strings"
)

// NonNil returns a copy of v in which every nil slice in a required field (no omitempty) is an
// empty slice, so it marshals as [] and never as null. The server passes every result, event
// and frame payload through it before it writes JSON. RawJSON and []byte are left alone.
func NonNil[T any](v T) T {
	rv := reflect.ValueOf(&v).Elem()
	fill(rv)
	return v
}

func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			fill(v.Elem())
		}
	case reflect.Interface:
		if !v.IsNil() {
			cp := reflect.New(v.Elem().Type()).Elem()
			cp.Set(v.Elem())
			fill(cp)
			v.Set(cp)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		for i := range v.Len() {
			fill(v.Index(i))
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			_, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			required := !strings.Contains(","+opts+",", ",omitempty,")
			if required && fv.Kind() == reflect.Slice && fv.IsNil() && f.Type.Elem().Kind() != reflect.Uint8 {
				fv.Set(reflect.MakeSlice(f.Type, 0, 0))
			}
			fill(fv)
		}
	}
}
