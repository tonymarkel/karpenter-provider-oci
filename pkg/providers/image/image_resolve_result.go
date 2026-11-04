/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package image

import (
	"github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	ocicore "github.com/oracle/oci-go-sdk/v65/core"
)

type ImageResolveResult struct {
	Images    []*ocicore.Image
	ImageType v1beta1.ImageType
	Os        *string
	OsVersion *string
	// ShapeCompatibility contains the selected image's limits when resolved for a shape.
	ShapeCompatibility *ocicore.ImageShapeCompatibilitySummary
}
