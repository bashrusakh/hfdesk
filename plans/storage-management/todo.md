# Todo

## Текущее состояние

- [x] Локальное и HF-удаление выбранной GGUF-группы реализовано; UI даёт выбрать место/группу и подтверждает область удаления во всех сохранённых версиях выбранного HF-репозитория в выбранном месте. Shared payload остаётся, если нужен другой сохранённой версии/группе.
- [x] Независимая браузерная и интеграционная проверка предыдущего кандидата прошла: проверялись локальные варианты/места и сохранение соседей/ссылок, HF-версии и общий blob, выбор/подтверждение/обновление UI и адаптивные размеры. Это не проверка последнего исправления.
- [x] Независимое review предыдущих исправлений завершено; прежний review исходного кандидата потребовал изменений, а последующие исправления были отдельно проверены. Это не закрывает review последнего изменения.
- [x] После интеграционного теста исправлены две подтверждённые ошибки: валидация назначения HF до admission и сохранение snapshot/blob, если unlink выбранной friendly-ссылки завершился ошибкой. Проверки пустого плана используют валидное HF-назначение.
- [x] Независимо проверить последнюю правку и полный кандидат `ead4c97a988e6b2a99a7e3590b693946ebcb1357` после неё: whole-diff review PASS с замечаниями без блокеров (26/26); свежая независимая browser/integration проверка прошла. Результаты привязаны к этому исходному HEAD и базе `97bdec0ba81b7ca30c3aba63315054eb273291ba`; см. `plan.md` §10.
- [x] Обычным merge включить свежий `origin/main` `50813f00554628b4146512b898ab10f7c6662efe` в ту же ветку PR #111; merge `112b809550e9da165115d6fba311712e6d1f9e57` сохраняет upstream-коммиты и исходную историю PR.
- [x] Добавить server integration regression для реальной пагинации Hub → полного плана job → защиты выбранного snapshot до передачи; `go test ./...`, `go test ./... -race`, `go vet ./...`, build и `git diff --check` прошли на кандидате `2a56d25` поверх base `50813f00554628b4146512b898ab10f7c6662efe`.
- [ ] Родительская сессия выполняет свежие независимые review и проверку полного кандидата после этих исправлений; все предыдущие evidence по более ранним base/head к нему не относятся. CodeQL на опубликованном HEAD по-прежнему показывает 7 High alerts: причины и статус не adjudicate-ились этой работой, закрытие/подавление не заявляется. Push и PR metadata остаются родительскими; PR Draft, Ready не разрешён.
- [x] Сообщение о P1 проверено и опровергнуто для указанного API-flow: обработчик не менял поведение при выключенном флаге; отдельные direct-library и Flat/friendly пути не доказывают этот P1. Изменение поведения флага не включалось и остаётся отдельным решением.
- [ ] Нативная Windows/SMB-проверка остаётся отдельным follow-up; здесь она не выполнялась. Внешние загрузки и исчерпывающий набор форматов также не проверялись.

Следующие пункты описывают исторические Package-этапы, а не текущую готовность. Их старые статусы намеренно сохранены как запись промежуточных состояний; для актуального результата см. раздел выше и `plan.md`.

## Исторический tracker Packages 1, 2A, 2B, GGUF filter correction, 3A и 3B backend

- [x] Удалить общую root/namespace/effect-инфраструктуру из продуктовых read paths; вернуть ограниченный код к поведению `origin/main`, включая Windows dedup identity.
- [x] Package 1: сохранить legacy DELETE-validator/friendly-cleanup и проверки main; не добавлять UI, quant API или quant-delete реализацию.
- [x] Проверить `go test ./internal/server ./pkg/hfdownloader`, `go test ./...` и `git diff --check`.
- [x] Package 2A: добавить read-only API списка конкретных мест и GGUF-групп, с выбором текущей локации и сохранением named HF snapshot versions; correction фиксирует полное filename identity, split template, link-only/HF warnings, typed local model lookup и регрессионные fixtures.
- [x] Package 2B: UI-выбор места/группы в Details реализован как read-only; кнопка destructive Delete остаётся disabled.
- [x] Package 3A: локальный `DELETE /api/cache-selection` удаляет только свежеподтверждённые GGUF members; старый whole-repository DELETE не переиспользован, HF write остаётся unsupported.
- [x] Исправить фильтр GGUF: несовпавший GGUF исключается и при отсутствии совпавшей GGUF в дереве; проверены обычный и LFS Q4/Q5.
- [x] Package 3A: writer exclusion для matching queued/new/active downloads и фактических rebuild/legacy-delete/mirror destination writes; matching queued jobs требуют ручной отмены, Q5 без совпадения не блокирует выбранный Q4.
- [x] Package 3B backend: selected HF snapshot/friendly entry deletion, shared-blob preservation, exact fresh confirmation, and HF repository/friendly writer reservation; focused pkg/server tests pass.
- [x] Parent-owned: verify current UI's HF-specific confirmation copy and browser flow now that backend advertises HF delete capability; perform integration review and review of whole candidate. Review and candidate verification passed; see `plan.md` §10.
- [ ] Parent-owned follow-up: native Windows/SMB verification. Not run; not a blocker for the completed local review/verification record.

Ниже сохранён исторический tracker предыдущего R/S/state/inventory-подхода. Его пункты и блокирующие статусы не описывают текущую цель; см. `plan.md`.

- [ ] Complete facts-only R on `feature/storage-root-ownership` from `0c876f38`.
  - [x] Preserve all pre-split WIP on local archive A `a9931836` (not an R
    ancestor; never push/cherry-pick wholesale).
  - [x] Remove destructive authorizers, deletion consumers and safety assertions
    from the R working tree; restore the legacy delete source block to `b4644b7`.
  - [x] Reconstruct bounded namespace/root/owned-entry facts and common read-side
    integration; retain logical IDs/roles, unknown Local type and error states.
  - [x] Replace native Linux positive permission check with real bind-region
    observation; keep isolation check and case-private fixture. Preserve the
    valid relative-route fixture for Windows.
  - [x] Run focused downloader/server tests; both pass on Linux Go 1.26.7.
  - [x] Run focused packages, `go test ./...`, `go test ./... -race`,
    `go vet ./...`, `go build ./cmd/hfdesk`, Windows test cross-builds and the
    existing Windows selector locally. All passed on Linux Go 1.26.7; cross-build
    and selector runs are not native Windows evidence.
  - [x] Attempt required-native `TestNativeMount*`; worker-marker negative case
    passed, but positive subprocess launch failed `operation not permitted` before
    namespace setup. No native mount/Go 1.25 pass is claimed.
  - [x] Scope the Windows R selector to facts only; retain the two legacy DELETE
    safety tests in source and exclude them from this facts-stage check.
  - [ ] Current candidate integrates fresh `origin/main` `8d3f982` and pins the
    required-native Linux job to Go 1.25; rerun native Linux/Windows CI after
    authorized publication. Prior remote run is stale; native Windows remains pending.
  - [x] Historical source commit `4bcf7f9` was reviewed as requiring changes;
    archive A is not its ancestor. That review is not approval of corrected R.
  - [x] Commit corrected source/test/CI bytes and status snapshot as
    `ff66636`; the completed candidate and status records remain below 5,000
    additions plus deletions.
  - [ ] Parent refreshes verification, reviews complete corrected R and rescope
    of owned Draft #111. Publication and Ready remain unauthorized.
- [ ] Build Safety S from reviewed R, selectively recover only bounded guards,
  destructive integration and safety tests from A, implement E and close F7.
- [ ] Start coordination/state work only after both root-stage foundations are
  reviewed and F7 is closed.
- [ ] Future inventory/delete engine; then API/UI/docs.

F7 remains blocking. No S/E implementation, PR update, push, Ready, merge or
publication is part of this task. A is intentionally retained locally for parent
handoff; its cleanup is not authorized before all unique components are accounted
for.
