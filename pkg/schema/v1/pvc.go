package v1

import (
	"github.com/icinga/icinga-kubernetes/pkg/database"
	"github.com/icinga/icinga-kubernetes/pkg/strcase"
	"github.com/icinga/icinga-kubernetes/pkg/types"
	kcorev1 "k8s.io/api/core/v1"
	kmetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
)

type Pvc struct {
	Meta
	Id           types.Binary
	Phase        string
	VolumeName   string
	VolumeMode   string
	StorageClass string
	Conditions   []PvcCondition `db:"-"`
	Labels       []Label        `db:"-"`
	PvcLabels    []PvcLabel     `db:"-"`
}

type PvcCondition struct {
	PvcId          types.Binary
	Type           string
	Status         string
	LastProbe      types.UnixMilli
	LastTransition types.UnixMilli
	Reason         string
	Message        string
}

type PvcLabel struct {
	PvcId   types.Binary
	LabelId types.Binary
}

func NewPvc() Resource {
	return &Pvc{}
}

func (p *Pvc) Obtain(k8s kmetav1.Object) {
	p.ObtainMeta(k8s)

	pvc := k8s.(*kcorev1.PersistentVolumeClaim)

	p.Id = types.Checksum(pvc.Namespace + "/" + pvc.Name)
	p.Phase = strcase.Snake(string(pvc.Status.Phase))
	p.VolumeName = pvc.Spec.VolumeName

	if pvc.Spec.VolumeMode != nil {
		p.VolumeMode = string(*pvc.Spec.VolumeMode)
	}

	if pvc.Spec.VolumeMode != nil {
		p.StorageClass = *pvc.Spec.StorageClassName
	}

	for _, condition := range pvc.Status.Conditions {
		p.Conditions = append(p.Conditions, PvcCondition{
			PvcId:          p.Id,
			Type:           strcase.Snake(string(condition.Type)),
			Status:         string(condition.Status),
			LastProbe:      types.UnixMilli(condition.LastProbeTime.Time),
			LastTransition: types.UnixMilli(condition.LastTransitionTime.Time),
			Reason:         condition.Reason,
			Message:        condition.Message,
		})
	}

	for labelName, labelValue := range pvc.Labels {
		labelId := types.Checksum(strings.ToLower(labelName + ":" + labelValue))
		p.Labels = append(p.Labels, Label{
			Id:    labelId,
			Name:  labelName,
			Value: labelValue,
		})
		p.PvcLabels = append(p.PvcLabels, PvcLabel{
			PvcId:   p.Id,
			LabelId: labelId,
		})
	}
}

func (p *Pvc) Relations() database.Relations {
	return database.Relations{
		database.HasMany[PvcCondition]{
			Entities:    p.Conditions,
			ForeignKey_: "pvc_id",
		},
		database.HasMany[Label]{
			Entities:    p.Labels,
			ForeignKey_: "value", // TODO: This is a hack to not delete any labels.
		},
		database.HasMany[PvcLabel]{
			Entities:    p.PvcLabels,
			ForeignKey_: "pvc_id",
		},
	}
}
