/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vfsclientset

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/acls"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/v1alpha2"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/util/pkg/vfs"
)

var StoreVersion = v1alpha2.SchemeGroupVersion

type ValidationFunction func(o runtime.Object) error

type VFSClientBase struct {
	kind       string
	vfsContext *vfs.VFSContext
	basePath   vfs.Path
	encoder    runtime.Encoder
	validate   ValidationFunction
}

func (c *VFSClientBase) Init(kind string, vfsContext *vfs.VFSContext, basePath vfs.Path, storeVersion runtime.GroupVersioner) {
	codecs := kopscodecs.Codecs
	yaml, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/yaml")
	if !ok {
		klog.Fatalf("no YAML serializer registered")
	}
	c.encoder = codecs.EncoderForVersion(yaml.Serializer, storeVersion)

	c.kind = kind
	c.vfsContext = vfsContext
	c.basePath = basePath
}

func (c *VFSClientBase) Find(ctx context.Context, name string) (runtime.Object, error) {
	o, err := c.readConfig(ctx, c.basePath.Join(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading %s %q: %v", c.kind, name, err)
	}
	return o, nil
}

func (c *VFSClientBase) List(ctx context.Context, items interface{}, options metav1.ListOptions) (interface{}, error) {
	return c.readAll(ctx, items)
}

func (c *VFSClientBase) create(ctx context.Context, cluster *kops.Cluster, i runtime.Object) error {
	objectMeta, err := meta.Accessor(i)
	if err != nil {
		return err
	}

	if c.validate != nil {
		err = c.validate(i)
		if err != nil {
			return err
		}
	}

	creationTimestamp := objectMeta.GetCreationTimestamp()
	if creationTimestamp.IsZero() {
		objectMeta.SetCreationTimestamp(metav1.NewTime(time.Now().UTC()))
	}

	err = c.writeConfig(ctx, cluster, c.basePath.Join(objectMeta.GetName()), i, vfs.WriteOptionCreate)
	if err != nil {
		if os.IsExist(err) {
			return err
		}
		return fmt.Errorf("error writing %s: %v", c.kind, err)
	}

	return nil
}

func (c *VFSClientBase) serialize(o runtime.Object) ([]byte, error) {
	var b bytes.Buffer
	err := c.encoder.Encode(o, &b)
	if err != nil {
		return nil, fmt.Errorf("error encoding object: %v", err)
	}

	return b.Bytes(), nil
}

func (c *VFSClientBase) readConfig(ctx context.Context, configPath vfs.Path) (runtime.Object, error) {
	data, err := configPath.ReadFile(ctx)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("error reading %s: %v", configPath, err)
	}

	object, _, err := kopscodecs.Decode(data, nil)
	if err != nil {
		return nil, fmt.Errorf("error parsing %s: %v", configPath, err)
	}
	return object, nil
}

func (c *VFSClientBase) writeConfig(ctx context.Context, cluster *kops.Cluster, configPath vfs.Path, o runtime.Object, writeOptions ...vfs.WriteOption) error {
	data, err := c.serialize(o)
	if err != nil {
		return fmt.Errorf("error marshaling object: %v", err)
	}

	create := false
	for _, writeOption := range writeOptions {
		switch writeOption {
		case vfs.WriteOptionCreate:
			create = true
		case vfs.WriteOptionOnlyIfExists:
			_, err = configPath.ReadFile(ctx)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("cannot update configuration file %s: does not exist", configPath)
				}
				return fmt.Errorf("error checking if configuration file %s exists already: %v", configPath, err)
			}
		default:
			return fmt.Errorf("unknown write option: %q", writeOption)
		}
	}

	acl, err := acls.GetACL(ctx, configPath, cluster)
	if err != nil {
		return err
	}

	rs := bytes.NewReader(data)
	if create {
		err = configPath.CreateFile(ctx, rs, acl)
	} else {
		err = configPath.WriteFile(ctx, rs, acl)
	}
	if err != nil {
		if create && os.IsExist(err) {
			klog.Warningf("failed to create file as already exists: %v", configPath)
			return err
		}
		return fmt.Errorf("error writing configuration file %s: %v", configPath, err)
	}
	return nil
}

func (c *VFSClientBase) update(ctx context.Context, cluster *kops.Cluster, i runtime.Object) error {
	objectMeta, err := meta.Accessor(i)
	if err != nil {
		return err
	}

	if c.validate != nil {
		err = c.validate(i)
		if err != nil {
			return err
		}
	}

	creationTimestamp := objectMeta.GetCreationTimestamp()
	if creationTimestamp.IsZero() {
		objectMeta.SetCreationTimestamp(metav1.NewTime(time.Now().UTC()))
	}

	err = c.writeConfig(ctx, cluster, c.basePath.Join(objectMeta.GetName()), i, vfs.WriteOptionOnlyIfExists)
	if err != nil {
		return fmt.Errorf("error writing %s: %v", c.kind, err)
	}

	return nil
}

func (c *VFSClientBase) delete(ctx context.Context, name string, options metav1.DeleteOptions) error {
	p := c.basePath.Join(name)
	err := p.Remove(ctx)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("error deleting %s configuration %q: %v", c.kind, name, err)
	}
	return nil
}

func (c *VFSClientBase) listNames(ctx context.Context) ([]string, error) {
	keys, err := listChildNames(ctx, c.basePath)
	if err != nil {
		return nil, fmt.Errorf("error listing %s in state store: %v", c.kind, err)
	}

	// Seems to be an assumption in k8s APIs that items are always returned sorted
	sort.Strings(keys)

	return keys, nil
}

func (c *VFSClientBase) readAll(ctx context.Context, items interface{}) (interface{}, error) {
	sliceValue := reflect.ValueOf(items)
	sliceType := reflect.TypeOf(items)
	if sliceType.Kind() != reflect.Slice {
		return nil, fmt.Errorf("expected slice, got %T", items)
	}

	names, err := c.listNames(ctx)
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		o, err := c.Find(ctx, name)
		if err != nil {
			return nil, err
		}

		if o == nil {
			return nil, fmt.Errorf("%s was listed, but then not found %q", c.kind, name)
		}

		sliceValue = reflect.Append(sliceValue, reflect.ValueOf(o).Elem())
	}

	return sliceValue.Interface(), nil
}
