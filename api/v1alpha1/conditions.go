/*
Copyright 2026.

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

package v1alpha1

// Condition types
const (
	ConditionReady                 = "Ready"
	ConditionIssued                = "Issued"
	ConditionUploaded              = "Uploaded"
	ConditionIssuerDefaultDiverged = "IssuerDefaultDiverged" // 不参与 Ready 聚合
	ConditionApplied               = "Applied"
	ConditionConflict              = "Conflict"
)

// AliyunCertificate reasons
const (
	ReasonNoIssuer                 = "NoIssuer"
	ReasonIssuerDefaultDiverged    = "IssuerDefaultDiverged"
	ReasonSecretNameConflict       = "SecretNameConflict"
	ReasonCertificateNotReady      = "CertificateNotReady"
	ReasonIssuanceStalled          = "IssuanceStalled"
	ReasonSecretNotFound           = "SecretNotFound"
	ReasonSecretInvalid            = "SecretInvalid"
	ReasonSelfSignedDuringIssuance = "SelfSignedDuringIssuance"
	ReasonSANsMismatch             = "SANsMismatch"
	ReasonCredentialsNotFound      = "CredentialsSecretNotFound"
	ReasonCredentialsInvalid       = "CredentialsInvalid"
	ReasonUploadFailed             = "UploadFailed"
	ReasonThrottled                = "Throttled"
	ReasonDeletionBlocked          = "DeletionBlockedByBindings"
	ReasonCleanupAbandoned         = "CleanupAbandoned"
	ReasonUploadDisabled           = "UploadDisabled"
	ReasonReady                    = "Ready"
)

// AliyunCertificateBinding reasons（Plan 2 使用，先定下来）
const (
	ReasonCertificateNotFound = "CertificateNotFound"
	ReasonTargetNotFound      = "TargetNotFound"
	ReasonDomainNotCovered    = "DomainNotCovered"
	ReasonConflictingBinding  = "ConflictingBinding"
	ReasonAccountMismatch     = "AccountMismatch"
	ReasonApplyFailed         = "ApplyFailed"
	ReasonDriftCorrected      = "DriftCorrected"
	ReasonApplied             = "Applied"
)

const (
	// FinalizerName 两个 CRD 共用
	FinalizerName = "certs.bestheme.ac.cn/finalizer"
	// LabelManaged 打在 operator 下发的 cert-manager Certificate 及其 Secret（经 secretTemplate）上
	LabelManaged = "certs.bestheme.ac.cn/managed"
)
