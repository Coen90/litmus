# Issue #5461 Analysis

**Litmus Agent Helm Chart: Pre-Install Hook Fails to Update Configs and Secrets on Application Sync**

- **URL**: https://github.com/litmuschaos/litmus/issues/5461
- **Author**: kabilesh13
- **Label**: bug
- **Status**: Open

---

## 1. Issue Summary

Litmus Agent Helm chart 설치 시 pre-install hook이 `install-litmuschaosagent` Job을 실행하여:

1. Chaos Center에 credentials, project ID, environment ID로 연결
2. Chaos Center에 agent 생성
3. `subscriber-config` (ConfigMap), `subscriber-secret` (Secret)에 연결 정보 저장

**문제**: 최초 설치 이후 application sync(ArgoCD 등)로 pre-install hook이 재실행되면, agent가 이미 존재한다고 판단하여 성공 메시지만 출력하고 **ConfigMap/Secret을 갱신하지 않음**. Pod 재시작 시 설정이 유실되어 연결이 끊어짐.

---

## 2. Root Cause Analysis

### 핵심 코드 흐름

**`subscriber.go:104-135` (init 함수)**

```go
isConfirmed, newKey, err := subscriberK8s.IsAgentConfirmed()
if err != nil {
    logrus.WithError(err).Fatal("Failed to check agent confirmed status")
}

if isConfirmed {
    infraData["ACCESS_KEY"] = newKey    // <-- BUG: 메모리에만 저장하고 끝
} else if !isConfirmed {
    // AgentConfirm → AgentRegister (ConfigMap/Secret 업데이트)
}
```

### Bug #1: `isConfirmed == true` 경로에서 ConfigMap/Secret 미갱신

| 경로 | `AgentRegister` 호출 | ConfigMap/Secret 갱신 |
|---|---|---|
| `isConfirmed == false` (최초 등록) | O | O |
| `isConfirmed == true` (재시작/re-sync) | **X** | **X** |

`AgentRegister()` (`operations.go:157-205`)는 ConfigMap과 Secret을 실제로 업데이트하는 함수이지만, `isConfirmed == true`일 때는 **호출되지 않음**.

### Bug #2: `IS_INFRA_CONFIRMED` infraData 미설정

`!isConfirmed` 경로에서는 `infraData["IS_INFRA_CONFIRMED"] = "true"`를 설정하지만, `isConfirmed` 경로에서는 이 값을 설정하지 않음.

### Bug #3: ConfigMap 삭제 시 복구 불가

`IsAgentConfirmed()`가 error를 반환하면(ConfigMap 삭제됨) `logrus.Fatal()`로 즉시 종료. 재등록을 시도하지 않음.

### `IsAgentConfirmed()` 로직 (`operations.go:130-155`)

```go
func (k8s *k8sSubscriber) IsAgentConfirmed() (bool, string, error) {
    getCM, err := clientset.CoreV1().ConfigMaps(InfraNamespace).Get(...)
    if k8s_errors.IsNotFound(err) {
        return false, "", errors.New("subscriber-config configmap not found")
    } else if getCM.Data["IS_INFRA_CONFIRMED"] == "true" {
        getSecret, err := clientset.CoreV1().Secrets(InfraNamespace).Get(...)
        if err != nil {
            return false, "", errors.New("subscriber-secret secret not found")
        }
        return true, string(getSecret.Data["ACCESS_KEY"]), nil
    }
    return false, "", nil
}
```

### `AgentRegister()` 로직 (`operations.go:157-205`)

```go
func (k8s *k8sSubscriber) AgentRegister(accessKey string) (bool, error) {
    getCM.Data["IS_INFRA_CONFIRMED"] = "true"
    clientset.CoreV1().ConfigMaps(InfraNamespace).Update(getCM)

    getSecret.StringData["ACCESS_KEY"] = accessKey
    clientset.CoreV1().Secrets(InfraNamespace).Update(getSecret)
    return true, nil
}
```

`AgentRegister`는 이미 Get -> Update 패턴의 idempotent 함수이므로, 재호출해도 부작용이 없음.

---

## 3. Affected Files

| File | Lines | Role |
|---|---|---|
| `chaoscenter/subscriber/subscriber.go` | 109-110 | `isConfirmed` 분기에서 `AgentRegister` 미호출 (핵심 버그) |
| `chaoscenter/subscriber/pkg/k8s/operations.go` | 130-155 | `IsAgentConfirmed()` - ConfigMap/Secret 존재 검증 |
| `chaoscenter/subscriber/pkg/k8s/operations.go` | 157-205 | `AgentRegister()` - ConfigMap/Secret 업데이트 |
| `chaoscenter/graphql/server/pkg/chaos_infrastructure/service.go` | 1059-1098 | (선택) 서버 측 re-sync 지원 |

---

## 4. Fix Proposals

### Proposal A: `isConfirmed == true` 경로에서 `AgentRegister` 호출 (권장)

**변경 범위**: `subscriber.go:109-110`

```go
// Before (buggy)
if isConfirmed {
    infraData["ACCESS_KEY"] = newKey
}

// After (fixed)
if isConfirmed {
    infraData["ACCESS_KEY"] = newKey
    infraData["IS_INFRA_CONFIRMED"] = "true"
    _, err = subscriberK8s.AgentRegister(infraData["ACCESS_KEY"])
    if err != nil {
        logrus.WithError(err).Fatal("Failed to register agent")
    }
}
```

- `AgentRegister`가 이미 idempotent하므로 부작용 없음
- 변경 범위가 가장 작음

### Proposal B: `IsAgentConfirmed()` 검증 로직 강화

**변경 범위**: `operations.go:130-155`

ConfigMap의 `IS_INFRA_CONFIRMED` 플래그만 확인하는 것이 아니라, ConfigMap과 Secret의 데이터 완전성까지 검증. 불완전하면 `false`를 반환하여 재등록 경로를 타게 함.

### Proposal C: 서버 측 Re-sync API 추가

**변경 범위**: `service.go:1059-1098`

이미 등록된 agent에 대해 최신 설정값을 반환하는 re-confirm/re-sync 엔드포인트 추가. 가장 근본적이지만 변경 범위가 큼.

---

## 5. Test Strategy

### 5.1 Unit Test (Failing Tests)

**파일**: `chaoscenter/subscriber/pkg/k8s/agent_flow_test.go`

`subscriber.go:104-135`의 로직을 `handleAgentConfirmation()` 함수로 추출하여 mock 기반 테스트 작성.

| Test | Status | Description |
|---|---|---|
| `TestAgentAlreadyConfirmed_ShouldCallAgentRegister` | **FAIL** | `isConfirmed == true`일 때 `AgentRegister` 미호출 증명 |
| `TestAgentAlreadyConfirmed_ShouldSetIsInfraConfirmed` | **FAIL** | `IS_INFRA_CONFIRMED` infraData 미설정 증명 |
| `TestAgentReSync_ConfigMapDeletedThenRestarted` | **FAIL** | ConfigMap 삭제 후 복구 불가 증명 |
| `TestAgentFirstRegistration_ShouldCallAgentRegister` | PASS | 최초 등록 정상 동작 확인 |
| `TestAgentFirstRegistration_ServerRejectsConfirm` | PASS | 서버 거부 시 정상 동작 확인 |

```
$ cd chaoscenter/subscriber && go test ./pkg/k8s/ -run "TestAgent" -v
```

**실행 결과**:

```
--- FAIL: TestAgentAlreadyConfirmed_ShouldCallAgentRegister (0.00s)
    Expected "AgentRegister" to have been called with:
        [existing-access-key]
    but actual calls were:
        []

--- FAIL: TestAgentAlreadyConfirmed_ShouldSetIsInfraConfirmed (0.00s)
    expected: "true"
    actual  : ""

--- FAIL: TestAgentReSync_ConfigMapDeletedThenRestarted (0.00s)
    Received unexpected error:
        failed to check agent confirmed status: subscriber-config configmap not found
    Messages: should recover from missing ConfigMap by re-confirming

--- PASS: TestAgentFirstRegistration_ShouldCallAgentRegister (0.00s)
--- PASS: TestAgentFirstRegistration_ServerRejectsConfirm (0.00s)
FAIL
```

### 5.2 Integration Test (Local K8s)

```bash
# 1. 로컬 클러스터 생성
kind create cluster --name litmus-test

# 2. Litmus 설치
helm install litmus-center <litmus-center-chart>
helm install litmus-agent <litmus-agent-chart>

# 3. 정상 설치 확인
kubectl get cm subscriber-config -n litmus -o yaml
kubectl get secret subscriber-secret -n litmus -o yaml

# 4. 장애 시뮬레이션: ConfigMap/Secret 삭제
kubectl delete cm subscriber-config -n litmus
kubectl delete secret subscriber-secret -n litmus

# 5. pod 재시작
kubectl rollout restart deployment subscriber -n litmus

# 6. 버그 확인: pod CrashLoopBackOff 또는 연결 실패
kubectl get pods -n litmus -l app=subscriber
kubectl logs -n litmus -l app=subscriber

# 7. ArgoCD re-sync 시뮬레이션
helm upgrade litmus-agent <litmus-agent-chart>
# → pre-install hook 재실행 후에도 ConfigMap/Secret 미복구 확인
```

### 5.3 Server-Side Fuzz Test

기존 fuzz 테스트로 서버 측 회귀 확인:

```bash
cd chaoscenter/graphql/server
go test ./pkg/chaos_infrastructure/fuzz/ -run FuzzConfirmInfraRegistration -fuzz=. -fuzztime=30s
```

---

## 6. Recommendation

**Proposal A**를 권장. 이유:

1. 변경이 `subscriber.go`의 3줄 추가로 최소화
2. `AgentRegister`가 이미 idempotent (Get -> Update)하므로 부작용 없음
3. 기존 최초 등록 경로에 영향 없음
4. 테스트가 이미 준비되어 있어 수정 후 즉시 검증 가능

수정 후 3개의 FAIL 테스트가 모두 PASS로 전환되는 것을 확인하면 검증 완료.
