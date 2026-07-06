{{/*
snakeKeys recursively rewrites a dict's keys from camelCase to snake_case via Sprig's snakecase.
Non-map values pass through unchanged. Used to convert a values.yaml-style dict (camelCase keys)
into the snake_case shape that the Temporal Go SDK envconfig TOML decoder expects.

Returns YAML; the caller round-trips through fromYaml to get the dict back.

Limitation: every nested map's keys are rewritten. Subtrees whose keys are user-supplied
identifiers (e.g. grpc_meta header names) must NOT be passed through this helper — attach them
after recursion.
*/}}
{{- define "tape-archiver-control-worker.snakeKeys" -}}
{{- $out := dict -}}
{{- range $k, $v := . -}}
  {{- $sk := snakecase $k -}}
  {{- if kindIs "map" $v -}}
    {{- $_ := set $out $sk (include "tape-archiver-control-worker.snakeKeys" $v | fromYaml) -}}
  {{- else -}}
    {{- $_ := set $out $sk $v -}}
  {{- end -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
triggerAuthentication renders the per-release keda.sh/v1alpha1 TriggerAuthentication that
backs the ScaledJob's Temporal trigger. It carries the KEDA-only Temporal credential (a
separate, least-privilege API key distinct from the worker's own config.temporal.apiKey)
and any keda.tls.* cert material, so KEDA can query task-queue backlog without ever being
handed the worker's credential.

Caller supplies a dict with keys "rootContext" (the chart root, post loader.init so bjw-s
metadata helpers resolve), "name" (the authRef name the ScaledJob's triggers point at),
"apiKeySecret" (dict {"name","key"} pointing at the Secret carrying the KEDA API key —
operator-supplied via secretKeyRef or chart-managed when config.temporal.keda.apiKey.value
is set), and "tlsSecrets" (a list of {"parameter","name","key"} entries for the optional
config.temporal.keda.tls.* material).

Emitted only on the type: scaledjob path; the gating happens in the caller (common.yaml)
so the default chart render stays byte-identical.
*/}}
{{- define "tape-archiver-control-worker.triggerAuthentication" -}}
{{- $rootContext := .rootContext -}}
{{- $name := .name -}}
{{- $apiKeySecret := .apiKeySecret -}}
{{- $tlsSecrets := .tlsSecrets | default list -}}
{{- $topLabels := include "bjw-s.common.lib.metadata.allLabels" $rootContext | fromYaml -}}
{{- $topAnnotations := include "bjw-s.common.lib.metadata.globalAnnotations" $rootContext | fromYaml -}}
---
apiVersion: keda.sh/v1alpha1
kind: TriggerAuthentication
metadata:
  name: {{ $name }}
  namespace: {{ $rootContext.Release.Namespace }}
  {{- with $topLabels }}
  labels:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
  {{- with $topAnnotations }}
  annotations:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
spec:
  secretTargetRef:
    - parameter: apiKey
      name: {{ $apiKeySecret.name }}
      key: {{ $apiKeySecret.key }}
    {{- range $entry := $tlsSecrets }}
    - parameter: {{ $entry.parameter }}
      name: {{ $entry.name }}
      key: {{ $entry.key }}
    {{- end }}
{{- end -}}

{{/*
scaledjob renders the keda.sh/v1alpha1 ScaledJob for the control worker. The Job pod
template is produced by the same bjw-s helpers the Deployment path uses
(bjw-s.common.lib.pod.spec / pod.metadata.labels / pod.metadata.annotations), so image,
env, volume mounts, securityContext, and the app.kubernetes.io/controller pod-template
label match what a Deployment for the same controller would have produced — the only
change is the deployment shape.

Caller supplies a dict with keys "rootContext" (the chart root, post loader.init),
"controllerName", "controller" (the merged controller values), "endpoint" /
"namespace" / "taskQueue" (the Temporal scaler target — the control worker polls the
single "control" queue with queueTypes workflow), "authRefName" (the
TriggerAuthentication name), and "kedaTLS" (a dict with optional "serverName" /
"unsafeSsl" scaler-side TLS metadata, already filtered for keda.tls.enabled upstream).

KEDA-level fields (pollingInterval, maxReplicaCount, …) lift from controller.keda.* onto
the ScaledJob spec; Job-level fields (backoffLimit, ttlSecondsAfterFinished, …) lift from
controller.job.* onto jobTargetRef.spec. maxReplicaCount defaults to 1 (the control worker
is a singleton — a control-queue backlog scales it 0 -> 1). The trigger metadata key names
(endpoint, namespace, taskQueue, queueTypes, tlsServerName, unsafeSsl) and lowercase values
match the KEDA Temporal scaler's struct tags, not chart-internal names.
*/}}
{{- define "tape-archiver-control-worker.scaledjob" -}}
{{- $rootContext := .rootContext -}}
{{- $controllerName := .controllerName -}}
{{- $endpoint := .endpoint -}}
{{- $namespace := .namespace -}}
{{- $taskQueue := .taskQueue -}}
{{- $authRefName := .authRefName -}}
{{- $kedaTLS := .kedaTLS | default dict -}}
{{- $fullName := include "bjw-s.common.lib.chart.names.fullname" $rootContext -}}

{{/* Synthesize the controller object bjw-s.common.lib.pod.spec expects: the controller
   values plus its identifier, and a Job-style restartPolicy default so the pod template
   is valid in a Job context. */}}
{{- $controllerObject := mustDeepCopy .controller -}}
{{- $_ := set $controllerObject "identifier" $controllerName -}}
{{- $pod := index $controllerObject "pod" | default dict -}}
{{- if not (hasKey $pod "restartPolicy") -}}
  {{- $_ := set $pod "restartPolicy" "Never" -}}
{{- end -}}
{{- $_ := set $controllerObject "pod" $pod -}}

{{- $resolvedName := printf "%s-%s" $fullName $controllerName | lower | trunc 63 | trimSuffix "-" -}}

{{- $topLabels := merge
  (dict "app.kubernetes.io/controller" $controllerName)
  (include "bjw-s.common.lib.metadata.allLabels" $rootContext | fromYaml)
-}}
{{- $topAnnotations := include "bjw-s.common.lib.metadata.globalAnnotations" $rootContext | fromYaml -}}

{{- $podLabels := include "bjw-s.common.lib.pod.metadata.labels" (dict "rootContext" $rootContext "controllerObject" $controllerObject) -}}
{{- $podAnnotations := include "bjw-s.common.lib.pod.metadata.annotations" (dict "rootContext" $rootContext "controllerObject" $controllerObject) -}}
{{- $podSpec := include "bjw-s.common.lib.pod.spec" (dict "rootContext" $rootContext "controllerObject" $controllerObject) -}}

{{- $kedaCfg := index $controllerObject "keda" | default dict -}}
{{- $jobCfg := index $controllerObject "job" | default dict -}}

{{/* Single Temporal trigger on the control queue. targetQueueSize / activationTarget-
   QueueSize default to 5 / 0: activation at any backlog (0 -> 1), one Job per 5 queued
   tasks up to maxReplicaCount. */}}
{{- $targetQueueSize := index $kedaCfg "targetQueueSize" | default 5 -}}
{{- $activationTargetQueueSize := index $kedaCfg "activationTargetQueueSize" | default 0 -}}
{{- $maxReplicaCount := index $kedaCfg "maxReplicaCount" | default 1 -}}
---
apiVersion: keda.sh/v1alpha1
kind: ScaledJob
metadata:
  name: {{ $resolvedName }}
  namespace: {{ $rootContext.Release.Namespace }}
  {{- with $topLabels }}
  labels:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
  {{- with $topAnnotations }}
  annotations:
    {{- range $k, $v := . }}
    {{ $k }}: {{ tpl $v $rootContext | quote }}
    {{- end }}
  {{- end }}
spec:
  {{- /* hasKey rather than `with` so an explicit zero (e.g. pollingInterval: 0) survives —
       Go template falsiness drops 0 with `with`, but KEDA accepts 0 as a valid value. */ -}}
  {{- if hasKey $kedaCfg "pollingInterval" }}
  pollingInterval: {{ get $kedaCfg "pollingInterval" }}
  {{- end }}
  {{- if hasKey $kedaCfg "successfulJobsHistoryLimit" }}
  successfulJobsHistoryLimit: {{ get $kedaCfg "successfulJobsHistoryLimit" }}
  {{- end }}
  {{- if hasKey $kedaCfg "failedJobsHistoryLimit" }}
  failedJobsHistoryLimit: {{ get $kedaCfg "failedJobsHistoryLimit" }}
  {{- end }}
  maxReplicaCount: {{ $maxReplicaCount }}
  {{- with $kedaCfg.scalingStrategy }}
  scalingStrategy: {{ . | toYaml | nindent 4 }}
  {{- end }}
  triggers:
    - type: temporal
      metadata:
        endpoint: {{ $endpoint | quote }}
        namespace: {{ $namespace | quote }}
        taskQueue: {{ $taskQueue | quote }}
        queueTypes: "workflow"
        targetQueueSize: {{ $targetQueueSize | quote }}
        activationTargetQueueSize: {{ $activationTargetQueueSize | quote }}
        {{- if $kedaTLS.serverName }}
        tlsServerName: {{ $kedaTLS.serverName | quote }}
        {{- end }}
        {{- if $kedaTLS.unsafeSsl }}
        unsafeSsl: "true"
        {{- end }}
      authenticationRef:
        name: {{ $authRefName }}
  jobTargetRef:
    {{- if hasKey $jobCfg "parallelism" }}
    parallelism: {{ get $jobCfg "parallelism" }}
    {{- end }}
    {{- if hasKey $jobCfg "completions" }}
    completions: {{ get $jobCfg "completions" }}
    {{- end }}
    {{- if hasKey $jobCfg "activeDeadlineSeconds" }}
    activeDeadlineSeconds: {{ get $jobCfg "activeDeadlineSeconds" }}
    {{- end }}
    {{- if hasKey $jobCfg "backoffLimit" }}
    backoffLimit: {{ get $jobCfg "backoffLimit" }}
    {{- end }}
    {{- if hasKey $jobCfg "ttlSecondsAfterFinished" }}
    ttlSecondsAfterFinished: {{ get $jobCfg "ttlSecondsAfterFinished" }}
    {{- end }}
    template:
      metadata:
        {{- with $podAnnotations }}
        annotations: {{ . | nindent 10 }}
        {{- end }}
        {{- with $podLabels }}
        labels: {{ . | nindent 10 }}
        {{- end }}
      spec: {{ $podSpec | nindent 8 }}
{{- end -}}
