//go:build !ignore_autogenerated

// Code generated by controller-gen. DO NOT EDIT.

package v1beta1

import (
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *GWireKNodeSpec) DeepCopyInto(out *GWireKNodeSpec) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	if in.UIDs != nil {
		in, out := &in.UIDs, &out.UIDs
		*out = make([]int, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new GWireKNodeSpec.
func (in *GWireKNodeSpec) DeepCopy() *GWireKNodeSpec {
	if in == nil {
		return nil
	}
	out := new(GWireKNodeSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *GWireKNodeSpec) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *GWireKNodeStatus) DeepCopyInto(out *GWireKNodeStatus) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	if in.GWireKItems != nil {
		in, out := &in.GWireKItems, &out.GWireKItems
		*out = make([]GWireStatus, len(*in))
		copy(*out, *in)
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new GWireKNodeStatus.
func (in *GWireKNodeStatus) DeepCopy() *GWireKNodeStatus {
	if in == nil {
		return nil
	}
	out := new(GWireKNodeStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *GWireKNodeStatus) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *GWireKObj) DeepCopyInto(out *GWireKObj) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Status.DeepCopyInto(&out.Status)
	in.Spec.DeepCopyInto(&out.Spec)
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new GWireKObj.
func (in *GWireKObj) DeepCopy() *GWireKObj {
	if in == nil {
		return nil
	}
	out := new(GWireKObj)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *GWireKObj) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto is an autogenerated deepcopy function, copying the receiver, writing into out. in must be non-nil.
func (in *GWireKObjList) DeepCopyInto(out *GWireKObjList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]GWireKObj, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

// DeepCopy is an autogenerated deepcopy function, copying the receiver, creating a new GWireKObjList.
func (in *GWireKObjList) DeepCopy() *GWireKObjList {
	if in == nil {
		return nil
	}
	out := new(GWireKObjList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject is an autogenerated deepcopy function, copying the receiver, creating a new runtime.Object.
func (in *GWireKObjList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
