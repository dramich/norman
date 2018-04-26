package crd

import (
	"context"
	"strings"
	"time"

	"fmt"

	"github.com/rancher/norman/store/proxy"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"github.com/sirupsen/logrus"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

type Factory struct {
	ClientGetter proxy.ClientGetter
}

func (c *Factory) AssignStores(ctx context.Context, storageContext types.StorageContext, schemas ...*types.Schema) error {
	schemaStatus, err := c.CreateCRDs(ctx, storageContext, schemas...)
	if err != nil {
		return err
	}

	for _, schema := range schemas {
		crd, ok := schemaStatus[schema]
		if !ok {
			return fmt.Errorf("failed to create create/find CRD for %s", schema.ID)
		}

		schema.Store = proxy.NewProxyStore(ctx, c.ClientGetter,
			storageContext,
			[]string{"apis"},
			crd.Spec.Group,
			crd.Spec.Version,
			crd.Status.AcceptedNames.Kind,
			crd.Status.AcceptedNames.Plural)
	}

	return nil
}

func (c *Factory) CreateCRDs(ctx context.Context, storageContext types.StorageContext, schemas ...*types.Schema) (map[*types.Schema]*apiext.CustomResourceDefinition, error) {
	schemaStatus := map[*types.Schema]*apiext.CustomResourceDefinition{}

	apiClient, err := c.ClientGetter.APIExtClient(nil, storageContext)
	if err != nil {
		return nil, err
	}

	ready, err := c.getReadyCRDs(apiClient)
	if err != nil {
		return nil, err
	}

	for _, schema := range schemas {
		crd, err := c.createCRD(apiClient, schema, ready)
		if err != nil {
			return nil, err
		}
		schemaStatus[schema] = crd
	}

	ready, err = c.getReadyCRDs(apiClient)
	if err != nil {
		return nil, err
	}

	for schema, crd := range schemaStatus {
		if readyCrd, ok := ready[crd.Name]; ok {
			schemaStatus[schema] = readyCrd
		} else {
			if err := c.waitCRD(ctx, apiClient, crd.Name, schema, schemaStatus); err != nil {
				return nil, err
			}
		}
	}

	return schemaStatus, nil
}

func (c *Factory) waitCRD(ctx context.Context, apiClient clientset.Interface, crdName string, schema *types.Schema, schemaStatus map[*types.Schema]*apiext.CustomResourceDefinition) error {
	logrus.Infof("Waiting for CRD %s to become available", crdName)
	defer logrus.Infof("Done waiting for CRD %s to become available", crdName)

	first := true
	return wait.Poll(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		if !first {
			logrus.Infof("Waiting for CRD %s to become available", crdName)
		}
		first = false

		crd, err := apiClient.ApiextensionsV1beta1().CustomResourceDefinitions().Get(crdName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}

		for _, cond := range crd.Status.Conditions {
			switch cond.Type {
			case apiext.Established:
				if cond.Status == apiext.ConditionTrue {
					schemaStatus[schema] = crd
					return true, err
				}
			case apiext.NamesAccepted:
				if cond.Status == apiext.ConditionFalse {
					logrus.Infof("Name conflict on %s: %v\n", crdName, cond.Reason)
				}
			}
		}

		return false, ctx.Err()
	})
}

func (c *Factory) createCRD(apiClient clientset.Interface, schema *types.Schema, ready map[string]*apiext.CustomResourceDefinition) (*apiext.CustomResourceDefinition, error) {
	plural := strings.ToLower(schema.PluralName)
	name := strings.ToLower(plural + "." + schema.Version.Group)

	crd, ok := ready[name]
	if ok {
		return crd, nil
	}

	crd = &apiext.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: apiext.CustomResourceDefinitionSpec{
			Group:   schema.Version.Group,
			Version: schema.Version.Version,
			Names: apiext.CustomResourceDefinitionNames{
				Plural: plural,
				Kind:   convert.Capitalize(schema.ID),
			},
		},
	}

	if schema.Scope == types.NamespaceScope {
		crd.Spec.Scope = apiext.NamespaceScoped
	} else {
		crd.Spec.Scope = apiext.ClusterScoped
	}

	logrus.Infof("Creating CRD %s", name)
	crd, err := apiClient.ApiextensionsV1beta1().CustomResourceDefinitions().Create(crd)
	if errors.IsAlreadyExists(err) {
		return crd, nil
	}
	return crd, err
}

func (c *Factory) getReadyCRDs(apiClient clientset.Interface) (map[string]*apiext.CustomResourceDefinition, error) {
	list, err := apiClient.ApiextensionsV1beta1().CustomResourceDefinitions().List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	result := map[string]*apiext.CustomResourceDefinition{}

	for i, crd := range list.Items {
		for _, cond := range crd.Status.Conditions {
			switch cond.Type {
			case apiext.Established:
				if cond.Status == apiext.ConditionTrue {
					result[crd.Name] = &list.Items[i]
				}
			}
		}
	}

	return result, nil
}
