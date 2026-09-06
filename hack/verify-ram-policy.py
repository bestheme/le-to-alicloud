#!/usr/bin/env python3
"""校验 RAM 策略的三处副本没有漂移。

README 的「RAM 权限」一节直接内联了完整策略 JSON（所有者要求：读者不该为了拼出
operator 需要哪些权限而跳两个文件）。代价是同一份内容有了多个可编辑的副本，而这类
文件漂移的后果是线上权限配错——所以由本脚本做门禁，而不是靠 README 里一句「记得同步」。

两条断言：

1. README「RAM 权限」一节里那个完整策略 JSON 块 == docs/ram/full-policy.json
2. certificate-cas-policy.json 与 binding-fc3-policy.json 的 Statement 按顺序拼起来
   == full-policy.json 的 Statement 列表

只用标准库：这是个门禁脚本，为它引入依赖等于给 CI 加一个可以自己坏掉的东西。
"""

import difflib
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
README = ROOT / "README.md"
FULL = ROOT / "docs" / "ram" / "full-policy.json"
CAS = ROOT / "docs" / "ram" / "certificate-cas-policy.json"
FC3 = ROOT / "docs" / "ram" / "binding-fc3-policy.json"

SECTION = "## RAM 权限"


def fail(msg):
    print("verify-ram-policy: " + msg, file=sys.stderr)
    sys.exit(1)


def diff(want_label, want, got_label, got):
    """把两个 JSON 值渲染成 unified diff。sort_keys 让键序差异不算差异。"""
    def render(v):
        return json.dumps(v, indent=2, sort_keys=True, ensure_ascii=False).splitlines(keepends=True)

    return "".join(difflib.unified_diff(
        render(want), render(got), fromfile=want_label, tofile=got_label))


def readme_policy():
    """抠出 README「RAM 权限」一节里的完整策略 JSON 块。

    只认「解析出来是个带 Statement 的对象」的块：同一节里还有一个示例 ARN 的 ```json
    块（内容是一个裸字符串），它也是合法 JSON，不能靠「解析得动」来区分。
    """
    text = README.read_text(encoding="utf-8")
    start = text.find("\n" + SECTION + "\n")
    if start < 0:
        fail("README.md 里找不到 `%s` 一节" % SECTION)
    body = text[start + 1:]
    nxt = re.search(r"^## ", body[len(SECTION):], re.M)
    if nxt:
        body = body[:len(SECTION) + nxt.start()]

    found = []
    for block in re.findall(r"```json\n(.*?)```", body, re.S):
        try:
            value = json.loads(block)
        except json.JSONDecodeError as e:
            fail("README「%s」一节里有个 json 代码块解析失败：%s" % (SECTION, e))
        if isinstance(value, dict) and "Statement" in value:
            found.append(value)
    if len(found) != 1:
        fail("README「%s」一节里应当恰好有 1 个完整策略 JSON 块，实际找到 %d 个"
             % (SECTION, len(found)))
    return found[0]


def load(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        fail("%s 解析失败：%s" % (path.relative_to(ROOT), e))


def main():
    full = load(FULL)

    inline = readme_policy()
    if inline != full:
        fail("README 里的完整策略与 docs/ram/full-policy.json 不一致：\n"
             + diff("docs/ram/full-policy.json", full, "README.md", inline))

    merged = load(CAS)["Statement"] + load(FC3)["Statement"]
    if merged != full["Statement"]:
        fail("certificate-cas-policy.json + binding-fc3-policy.json 的 Statement "
             "合并后与 full-policy.json 不一致：\n"
             + diff("docs/ram/full-policy.json", full["Statement"],
                    "cas + fc3 合并", merged))

    print("verify-ram-policy: OK（README 与 docs/ram/ 下三份策略一致）")


if __name__ == "__main__":
    main()
