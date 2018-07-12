package action

import (
	"errors"
	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/sirupsen/logrus"
	"github.com/syndesisio/syndesis-operator/pkg/apis/syndesis/v1alpha1"
	syndesistemplate "github.com/syndesisio/syndesis-operator/pkg/syndesis/template"
	"github.com/syndesisio/syndesis-operator/pkg/syndesis/version"
	"github.com/syndesisio/syndesis-operator/pkg/util"
	"k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"time"
)

const (
	UpgradePodServiceAccountName = "syndesis-operator"
)

// Upgrades Syndesis to the version supported by this operator using the upgrade template.
type Upgrade struct {
	operatorVersion	string
}

func (a *Upgrade) CanExecute(syndesis *v1alpha1.Syndesis) bool {
	return syndesisInstallationStatusIs(syndesis, v1alpha1.SyndesisInstallationStatusUpgrading)
}

func (a *Upgrade) Execute(syndesis *v1alpha1.Syndesis) error {
	if a.operatorVersion == "" {
		operatorVersion, err := version.GetSyndesisVersionFromOperatorTemplate()
		if err != nil {
			return err
		}
		a.operatorVersion = operatorVersion
	}

	namespaceVersion, err := version.GetSyndesisVersionFromNamespace(syndesis.Namespace)
	if err != nil {
		return err
	}
	targetVersion := a.operatorVersion

	resources, err := a.getUpgradeResources(syndesis)
	if err != nil {
		return err
	}

	templateUpgradePod, err := a.findUpgradePod(resources)
	if err != nil {
		return err
	}

	upgradePod, err := a.getUpgradePodFromNamespace(templateUpgradePod, syndesis)
	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	}

	if syndesis.Status.ForceUpgrade || k8serrors.IsNotFound(err) {
		// Upgrade pod not found or upgrade forced

		if namespaceVersion != targetVersion {
			logrus.Info("Upgrading syndesis resource ", syndesis.Name, " from version ", namespaceVersion, " to ", targetVersion)

			// Set the correct service account for the upgrade pod
			templateUpgradePod.Spec.ServiceAccountName = UpgradePodServiceAccountName

			for _, res := range resources {
				setNamespaceAndOwnerReference(res, syndesis)

				err = createOrReplaceForce(res, true)
				if err != nil {
					return err
				}
			}

			if syndesis.Status.ForceUpgrade {
				target := syndesis.DeepCopy()
				target.Status.ForceUpgrade = false

				return sdk.Update(target)
			} else {
				return nil
			}
		} else {
			// No upgrade pod, no version change: upgraded
			logrus.Info("Syndesis resource ", syndesis.Name, " already upgraded to version ", targetVersion)
			return upgradeCompleted(syndesis, targetVersion)
		}
	} else {
		// Upgrade pod present, checking the status
		if upgradePod.Status.Phase == v1.PodSucceeded {
			// Upgrade finished (correctly)

			// Getting the namespace version again for double check
			newNamespaceVersion, err := version.GetSyndesisVersionFromNamespace(syndesis.Namespace)
			if err != nil {
				return err
			}

			if newNamespaceVersion == targetVersion {
				logrus.Info("Syndesis resource ", syndesis.Name, " upgraded to version ", targetVersion)
				return upgradeCompleted(syndesis, targetVersion)
			} else {
				logrus.Warn("Upgrade pod terminated successfully but Syndesis version (", newNamespaceVersion, ") does not reflect target version (", targetVersion, ") for resource ", syndesis.Name, ". Forcing upgrade.")
				target := syndesis.DeepCopy()
				target.Status.ForceUpgrade = true

				return sdk.Update(target)
			}
		} else if upgradePod.Status.Phase == v1.PodFailed {
			// Upgrade failed
			logrus.Warn("Failure while upgrading Syndesis resource ", syndesis.Name, " to version ", targetVersion, ": upgrade pod failure")

			target := syndesis.DeepCopy()
			target.Status.InstallationStatus = v1alpha1.SyndesisInstallationStatusUpgradeFailureBackoff
			target.Status.Reason = v1alpha1.SyndesisStatusReasonUpgradePodFailed
			target.Status.LastUpgradeFailure = &metav1.Time{
				Time: time.Now(),
			}
			target.Status.UpgradeAttempts = target.Status.UpgradeAttempts + 1

			return sdk.Update(target)
		} else {
			// Still running
			logrus.Info("Syndesis resource ", syndesis.Name, " is currently being upgraded to version ", targetVersion)
			return nil
		}

	}

}

func upgradeCompleted(syndesis *v1alpha1.Syndesis, newVersion string) error {
	target := syndesis.DeepCopy()
	target.Status.InstallationStatus = v1alpha1.SyndesisInstallationStatusInstalled
	target.Status.Reason = v1alpha1.SyndesisStatusReasonMissing
	target.Status.Version = newVersion
	target.Status.LastUpgradeFailure = nil
	target.Status.UpgradeAttempts = 0
	target.Status.ForceUpgrade = false

	return sdk.Update(target)
}

func (a *Upgrade) getUpgradeResources(syndesis *v1alpha1.Syndesis) ([]runtime.Object, error) {
	rawResources, err := syndesistemplate.GetUpgradeResources(syndesis, syndesistemplate.UpgradeParams{
		InstallParams: syndesistemplate.InstallParams{
			OAuthClientSecret: "-",
		},
		SyndesisVersion: a.operatorVersion,
	})
	if err != nil {
		return nil, err
	}

	resources := make([]runtime.Object, 0)
	for _, obj := range rawResources {
		res, err := util.LoadKubernetesResource(obj.Raw)
		if err != nil {
			return nil, err
		}
		resources = append(resources, res)
	}

	return resources, nil
}

func (a *Upgrade) findUpgradePod(resources []runtime.Object) (*v1.Pod, error) {
	for _, res := range resources {
		if pod, ok := res.(*v1.Pod); ok {
			return pod, nil
		}
	}
	return nil, errors.New("upgrade pod not found")
}

func (a *Upgrade) getUpgradePodFromNamespace(podTemplate *v1.Pod, syndesis *v1alpha1.Syndesis) (*v1.Pod, error) {
	pod := v1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: podTemplate.APIVersion,
			Kind: podTemplate.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: syndesis.Namespace,
			Name: podTemplate.Name,
		},
	}

	err := sdk.Get(&pod)
	return &pod, err
}