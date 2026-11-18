# torrent

Небольшая утилита на чистом Go для работы с торрент-файлами:

- просмотр метаданных v1, v2 и hybrid v1+v2;
- вывод в JSON для `jq`;
- редактирование любых ключей, включая вложенные и нестандартные;
- сохранение `info`-секции в исходном виде при редактировании за её пределами;
- создание v1, v2 и гибридных торрентов;
- проверка скачанных данных по хешам;
- создание pad-файлов для старых клиентов;
- экспорт/импорт торрента в редактируемый JSON.

Создано с помощью DeepSeek V4.1 Flash 1017 и агента OpenCode.

Зависимостей нет, используется только стандартная библиотека Go. На выходе —
один статический бинарник, который легко собрать под Linux, Windows, macOS и
разные архитектуры.

## Сборка

```sh
go build -trimpath -ldflags "-s -w" -o torrent .
```

Есть `Makefile`:

```sh
make build        # локальный бинарник torrent
make build-all    # сборка под Linux/Windows/macOS в dist/
```

Кросс-компиляция вручную, например:

```sh
CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o torrent-linux-amd64 .
CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o torrent-linux-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o torrent-windows-amd64.exe .
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o torrent-darwin-arm64 .
```

Тесты:

```sh
go test ./...
```

## Структура исходников

Исходники разделены по файлам, но остаются в одном `package main`.
На конечный результат это не влияет: `go build` по-прежнему создаёт один
самодостаточный бинарник.

```text
main.go      — точка входа, диспетчер команд, справка
bencode.go   — bencode, raw info, JSON-представление значений
torrent.go   — анализ v1/v2/hybrid, трекеры, предупреждения
hash.go      — v1 piece hashes, Merkle-дерево и v2 piece layers
cli.go       — флаги, атомарная запись, форматирование
info.go      — команда info
edit.go      — пути, типы значений, команда edit
create.go    — создание v1/v2/hybrid
verify.go    — проверка данных
repair.go    — восстановление недостающих v2 piece layers
pad.go       — padding-файлы
jsonio.go    — export/import JSON
```

Внешних зависимостей нет. Специально не подключаются сторонние торрент- или
bencode-библиотеки: утилите нужны точное сохранение сырых байтов `info` и
редактирование произвольных ключей, которые типичные библиотеки не
гарантируют.

## Команды

```text
torrent info FILE...       показать метаданные
torrent edit FILE          изменить ключи
torrent create PATH...     создать торрент
torrent verify FILE...     проверить данные
torrent repair FILE...     восстановить недостающие v2 piece layers
torrent pad FILE           создать pad-файлы
torrent export FILE        выгрузить торрент в JSON
torrent import FILE.json   собрать торрент из JSON
```

У каждой команды есть `--help`.

## Просмотр информации

```sh
torrent info file.torrent
```

Выводятся версия, info hash (v1 и/или v2), длина и количество кусков, размер,
список файлов, трекеры, дата создания и нестандартные ключи. Отсутствие
`announce` не считается ошибкой: утилита смотрит также `announce-list` и
показывает предупреждение, что торрент может работать через DHT/PEX.

По умолчанию показываются первые 30 файлов; `--files` выводит все,
`--no-files` скрывает список, а `--no-extra` скрывает нестандартные ключи.

JSON:

```sh
torrent info --json file.torrent | jq .
torrent info --json --compact file.torrent | jq -r '.name'
torrent info --json --compact file.torrent | jq -r '.trackers.all[]'
torrent info --json file.torrent | jq '.files[] | select(.padding != true) | .path'
```

Несколько файлов:

```sh
torrent info --json *.torrent | jq '.[] | {name, v1: .info_hash_v1, v2: .info_hash_v2}'
```

Если передан один файл, выводится JSON-объект, если несколько — массив.

Основные поля JSON:

- `file`, `name`, `version` (`["v1"]`, `["v2"]` или `["v1","v2"]`);
- `info_hash_v1`, `info_hash_v2`;
- `piece_length`, `piece_count`, `piece_count_v1`, `piece_count_v2`;
- `total_size`, при наличии padding — `total_size_with_padding`;
- `private`, `source`, `creation_date`, `created_by`, `comment`;
- `trackers.announce`, `trackers.tiers`, `trackers.all`;
- `files[]` с полями `path`, `length`, `offset`, `padding`, `attr`,
  `pieces_root`, `sha1`;
- `piece_layers` — краткая сводка (полное содержимое доступно через `--raw`);
- `extra.top` и `extra.info` — нестандартные ключи;
- `warnings[]` — предупреждения.

Бинарные bencode-строки в JSON представляются как `{"$base64": "..."}`.
Флаг `--raw` добавляет полное дерево bencode в поле `bencode`.

## Редактирование

По умолчанию результат пишется в stdout, если это не терминал. Явно:

```sh
torrent edit file.torrent --output fixed.torrent ...
torrent edit --in-place file.torrent ...     # или -i
```

Примеры:

```sh
# Добавить трекер (новым tier) и announce, если его не было
torrent edit -i file.torrent --tracker udp://tracker.example:6969/announce

# Заменить список трекеров
torrent edit -i file.torrent --replace-trackers \
  --tracker udp://tracker1.example/announce \
  --tracker udp://tracker2.example/announce

# Удалить трекер
torrent edit -i file.torrent --remove-tracker udp://old.example/announce

# Произвольные ключи
torrent edit -i file.torrent \
  --set comment="new comment" \
  --set info.source=example \
  --set-int info.private=1 \
  --set-json 'custom={"a":[1,2,3]}'

# Вложенные пути и списки
torrent edit -i file.torrent --set 'custom.nested.key=value'
torrent edit -i file.torrent --set 'announce-list[0][0]=udp://new.example/announce'
torrent edit -i file.torrent --append 'custom.list=item'
torrent edit -i file.torrent --unset 'custom.nested.key'
```

Пути:

- `.` разделяет ключи словарей;
- `[N]` обращается к элементам списков;
- ключ с точкой или другим спецсимволом можно записать как `["key.with.dots"]`;
- точку внутри ключа можно экранировать: `key\.with\.dots`.

Типы значений для `--set` и `--append`:

| Префикс | Значение |
|---|---|
| `int:42` | целое число |
| `str:42` | строка (полезно, если значение начинается с префикса) |
| `b64:...` | бинарные данные из base64 |
| `hex:...` | бинарные данные из hex |
| `file:PATH` | содержимое файла |
| `json:...` | JSON-значение (объект, массив, число, строка) |
| `bencode:...` | литерал bencode |
| без префикса | UTF-8 строка |

Для `--set` есть также отдельные флаги `--set-int`, `--set-json`,
`--set-bencode`.

### Сохранение info hash

При загрузке утилита запоминает точные байты значения `info`. Если изменения
затрагивают только верхний уровень (например, `announce`, `announce-list`,
`comment`), при записи эти байты подставляются обратно без перекодирования.
Поэтому ни v1, ни v2 info hash не меняются. Это работает и для файлов с
неканоническим порядком ключей внутри `info`.

Если изменить что-либо внутри `info`, hash закономерно изменится. В этом
случае `info` перекодируется в каноническом виде (ключи словарей
сортируются). Утилита печатает старый и новый hash в stderr.

## Создание торрентов

```sh
# v1 (по умолчанию)
torrent create -o out.torrent ./data

# v2
torrent create --v2 -o out.torrent ./data

# гибрид v1+v2
torrent create --hybrid -o out.torrent ./data
```

Полезные флаги:

```text
--piece-length, -p   16KiB, 256KiB, 1MiB ... (по умолчанию подбирается)
--name NAME          имя торрента
--announce URL       announce
--tracker URL        добавить в announce-list (можно повторять)
--private            info.private = 1
--source TEXT        info.source
--comment TEXT       comment
--created-by TEXT    created by
--no-created-by      не добавлять created by
--creation-date N    Unix timestamp
--no-creation-date   не добавлять creation date
--set PATH=VALUE     дополнительный/нестандартный ключ
```

Для гибридных торрентов в v1-список автоматически вставляются padding-файлы
(`attr: "p"`, путь `.pad/<размер>`), чтобы файлы начинались с границ кусков.
Для v2 строятся `file tree` и `piece layers` по BEP 52.

## Проверка данных

```sh
torrent verify --data /downloads out.torrent
```

`--data DIR` — каталог, в котором лежит верхнеуровневое имя торрента. Например,
если торрент называется `ubuntu` и содержит `ubuntu/file1`, `ubuntu/dir/file2`,
то `--data /downloads` означает `/downloads/ubuntu/file1` и
`/downloads/ubuntu/dir/file2`. Для однофайлового торрента проверяется
`/downloads/<name>`.

Проверяются:

- v1: все куски SHA-1 по порядку, включая padding-файлы (их содержимое
  считается нулями, физического файла может не быть);
- v2: корни Меркла каждого файла и `piece layers`;
- hybrid: и v1, и v2.

Код возврата `2` означает, что проверка не прошла. Есть `--json` для
машинной обработки.

## Восстановление piece layers

Если у v2 или hybrid-торрента отсутствует секция `piece layers`, её можно
восстановить по уже скачанным файлам:

```sh
torrent repair --data /downloads saved.torrent
torrent repair --data /downloads -o fixed.torrent saved.torrent
```

По умолчанию создаётся `<input>.repaired.torrent`, исходный файл не
изменяется. Для нескольких файлов `--out` должен быть каталогом.

Опции:

```text
-o, --out PATH       выходной файл или каталог
-f, --force          перезаписать существующий выходной файл
    --data DIR       каталог, содержащий верхнеуровневое имя торрента
    --recompute      пересчитать все layers, а не только отсутствующие
-j, --json           JSON-результат
-q, --quiet          не выводить прогресс
```

Что делает команда:

- для v1-торрента ничего не пишет: piece layers ему не нужны;
- для v2/hybrid находит v2-файлы длиннее `piece length`, которым нужен layer;
- если layer отсутствует, имеет неверную длину или указан `--recompute`,
  вычисляет его по файлу;
- проверяет, что вычисленный `pieces root` совпадает с корнем из `file tree`;
- записывает новую секцию `piece layers`, **не изменяя `info`** и info hash;
- если все layers уже полные, выходной файл не создаётся.

В отличие от `magnet repair`, команда работает полностью офлайн: она не
обращается к DHT или пирам, а пересчитывает layers по локальным данным.
Если файл отсутствует, имеет неправильный размер или его содержимое не
соответствует `pieces root`, команда завершается с ошибкой и не пишет
выходной торрент. Восстановить piece layers торрент-файла онлайн по данным
из сети можно с помощью [magnet](https://github.com/shurshur/magnet)

`--recompute` полезен, если layers присутствуют, но есть подозрение, что они
повреждены: команда заново посчитает их по данным и заменит отличающиеся.

JSON-режим выводит массив результатов, как `magnet repair`:

```json
[
  {
    "input": "saved.torrent",
    "output": "saved.repaired.torrent",
    "version": "hybrid",
    "state": "found",
    "layers_total": 12,
    "layers_added": 12
  }
]
```

## Pad-файлы

```sh
torrent pad --dir /downloads out.torrent
torrent pad --dir /downloads --dry-run out.torrent
```

Создаются только файлы с `attr: "p"` (обычно в гибридных торрентах), нужного
размера, заполненные нулями. Файлы создаются разреженными, если файловая
система это поддерживает. Существующие файлы правильного размера
пропускаются; для перезаписи используйте `--force`.

## Экспорт и импорт JSON

```sh
torrent export -o file.json file.torrent
jq '.announce = "udp://new.example/announce"' file.json > edited.json
torrent import -o fixed.torrent edited.json
```

Экспорт — это обычный JSON-объект с ключами торрента плюс служебный ключ
`__torrenttool`. В нём сохраняются исходные байты `info` и отпечаток
`info`-значения. Если при редактировании JSON менять только данные вне `info`,
при импорте исходные байты `info` используются повторно и info hash не
меняется. Если изменить `info`, утилита сообщит об этом и перекодирует его.

Бинарные строки представляются объектами `{"$base64": "..."}`. Целые числа —
обычными JSON-числами. `null` и `true`/`false` в bencode не существуют,
поэтому при импорте они не поддерживаются (используйте `1`/`0` или удаление
ключа).

## Ограничения и замечания

- Символические ссылки при создании торрентов не поддерживаются.
- При создании v2-торрента из каталога с одним файлом в корне эвристика
  определяет однофайловый торрент по совпадению `name` и имени файла в
  `file tree`. Это соответствует BEP 52, но полностью неоднозначный случай
  (каталог и файл с одинаковым именем) теоретически возможен.
- `verify` не проверяет `sha1`-подсказки BEP 47; каноническими считаются
  piece hashes.
- `repair` работает только по локальным файлам и не получает piece layers из
  сети. Если в `info` отсутствует или повреждён `pieces root`, восстановить
  слой нельзя: это потребовало бы изменить `info` и info hash.
- Без `--recompute` команда `repair` заполняет только отсутствующие или
  структурно неполные layers; уже существующие layers корректной длины не
  пересчитываются.
- JSON-числа больше 2^53 могут потерять точность при обработке некоторыми
  JSON-инструментами (например, `jq`). Для торрентов это редкость.
- `--data` указывает на родительский каталог верхнеуровневого имени, а не на
  сам каталог с данными.

## Внутреннее устройство

- Свой bencode-парсер/энкодер без зависимостей.
- Для `info` хранится точный срез исходных байтов; при записи он
  подставляется в верхнеуровневый словарь.
- v1 piece hashes считаются SHA-1 по потоку файлов (с нулями для padding).
- `repair` сохраняет существующие записи `piece layers`, пересчитывает только
  отсутствующие/неполные (или все с `--recompute`), сверяет корни файлов и
  записывает результат без изменения `info`.
- v2 использует дерево Меркла BEP 52: листья — SHA-256 блоков по 16 KiB,
  последний блок может быть короче; недостающие листья заполняются нулями;
  `piece layers` содержат хеши уровня куска; дерево файла балансируется
  хешем полностью нулевого куска.
- `piece layers` и корни файлов проверены на совместимость с эталонным
  генератором BEP 52.

## Лицензия

Проект распространяется под лицензией **MIT**. Полный текст — в файле
[LICENSE](LICENSE).
