#!/usr/bin/env python3
"""校验 RAM 策略的六处副本没有漂移。

README 的「RAM 权限」一节直接内联了完整策略 JSON（所有者要求：读者不该为了拼出
operator 需要哪些权限而跳两个文件），spec §8.3 也留着同一份策略。代价是同一份内容有了
多份可编辑的副本，而这类文件漂移的后果是线上权限配错——所以由本脚本做门禁，而不是靠
文档里一句「记得同步」。

三条断言：

1. README「RAM 权限」一节里那个完整策略 JSON 块 == docs/ram/full-policy.json
2. spec §8.3 里那个完整策略 JSON 块 == docs/ram/full-policy.json
3. certificate-cas-policy.json、binding-fc3-policy.json、binding-oss-policy.json 的
   Statement 按顺序拼起来 == full-policy.json 的 Statement 列表

只用标准库：这是个门禁脚本，为它引入依赖等于给 CI 加一个可以自己坏掉的东西。
"""

import difflib
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
FULL = ROOT / "docs" / "ram" / "full-policy.json"
CAS = ROOT / "docs" / "ram" / "certificate-cas-policy.json"
FC3 = ROOT / "docs" / "ram" / "binding-fc3-policy.json"
OSS = ROOT / "docs" / "ram" / "binding-oss-policy.json"

README = ROOT / "README.md"
README_SECTION = "## RAM 权限"
SPEC = ROOT / "docs" / "superpowers" / "specs" / "2026-09-04-le-to-alicloud-operator-design.md"
SPEC_SECTION = "### 8.3 阿里云 RAM 最小权限"


def fail(msg):
    print("verify-ram-policy: " + msg, file=sys.stderr)
    sys.exit(1)


def diff(want_label, want, got_label, got):
    """把两个 JSON 值渲染成 unified diff。sort_keys 让键序差异不算差异。"""
    def render(v):
        return json.dumps(v, indent=2, sort_keys=True, ensure_ascii=False).splitlines(keepends=True)

    return "".join(difflib.unified_diff(
        render(want), render(got), fromfile=want_label, tofile=got_label))


def section(path, heading):
    """截出一节：从 heading 那一行到下一个同级或更高级标题为止。"""
    text = path.read_text(encoding="utf-8")
    start = text.find("\n" + heading + "\n")
    if start < 0:
        fail("%s 里找不到 `%s` 一节" % (path.relative_to(ROOT), heading))
    body = text[start + 1:]
    level = len(heading) - len(heading.lstrip("#"))
    nxt = re.search(r"^#{1,%d} " % level, body[len(heading):], re.M)
    if nxt:
        body = body[:len(heading) + nxt.start()]
    return body


def policy_in(path, heading):
    """抠出一节里的完整策略 JSON 块。

    只认「解析出来是个带 Statement 的对象」的块：README 那一节里还有一个示例 ARN 的
    ```json 块（内容是一个裸字符串），它也是合法 JSON，不能靠「解析得动」来区分。
    """
    where = "%s「%s」一节" % (path.relative_to(ROOT), heading.lstrip("# "))
    found = []
    for block in re.findall(r"```json\n(.*?)```", section(path, heading), re.S):
        try:
            value = json.loads(block)
        except json.JSONDecodeError as e:
            fail("%s里有个 json 代码块解析失败：%s" % (where, e))
        if isinstance(value, dict) and "Statement" in value:
            found.append(value)
    if len(found) != 1:
        fail("%s里应当恰好有 1 个完整策略 JSON 块，实际找到 %d 个" % (where, len(found)))
    return found[0]


def load(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        fail("%s 解析失败：%s" % (path.relative_to(ROOT), e))


def main():
    full = load(FULL)

    for path, heading in ((README, README_SECTION), (SPEC, SPEC_SECTION)):
        inline = policy_in(path, heading)
        if inline != full:
            fail("%s 里的完整策略与 docs/ram/full-policy.json 不一致：\n%s"
                 % (path.relative_to(ROOT), diff(
                     "docs/ram/full-policy.json", full, str(path.relative_to(ROOT)), inline)))

    merged = load(CAS)["Statement"] + load(FC3)["Statement"] + load(OSS)["Statement"]
    if merged != full["Statement"]:
        fail("certificate-cas-policy.json + binding-fc3-policy.json + binding-oss-policy.json "
             "的 Statement 合并后与 full-policy.json 不一致：\n"
             + diff("docs/ram/full-policy.json", full["Statement"],
                    "cas + fc3 + oss 合并", merged))

    print("verify-ram-policy: OK（README、spec §8.3 与 docs/ram/ 下四份策略一致）")


if __name__ == "__main__":
    main()
