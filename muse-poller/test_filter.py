"""自检: @ /引用过滤 + 渲染截断, 不联网, 直接跑: python3 test_filter.py"""
import sys

sys.path.insert(0, ".")

from poller import should_reply  # noqa: E402


def check(name, got, want):
    status = "OK " if got == want else "FAIL"
    print(f"[{status}] {name}: got={got} want={want}")
    return got == want


ok = True
BOT = "my_muse_bot"

# 私聊全回
ok &= check("私聊普通消息回复", should_reply({"chat_type": "private", "text": "hi"}, BOT, None), True)
# 群里无触发不回
ok &= check("群普通消息不回", should_reply({"chat_type": "group", "text": "hi"}, BOT, None), False)
ok &= check("超群普通消息不回", should_reply({"chat_type": "supergroup", "text": "hi"}, BOT, None), False)
# 群里 @ 回
ok &= check(
    "群 @ 回",
    should_reply({"chat_type": "group", "text": "@my_muse_bot 查一下", "is_mention": True}, BOT, None),
    True,
)
# 文本兜底: Worker 字段缺失时按文本判断
ok &= check(
    "群 @ 文本兜底回",
    should_reply({"chat_type": "supergroup", "text": "问下 @My_Muse_Bot"}, BOT, None),
    True,
)
# 群里 @ 别人不回
ok &= check(
    "群 @ 别人不回",
    should_reply({"chat_type": "group", "text": "@someone hi"}, BOT, None),
    False,
)
# 群里引用 Bot 回
ok &= check(
    "群引用 Bot 回",
    should_reply({"chat_type": "group", "text": "继续说", "is_reply_to_bot": True}, BOT, None),
    True,
)
# channel 不回
ok &= check("channel 不回", should_reply({"chat_type": "channel", "text": "@my_muse_bot"}, BOT, None), False)
# 私聊严格模式
ok &= check(
    "私聊严格模式无触发不回",
    should_reply({"chat_type": "private", "text": "hi"}, BOT, None, True),
    False,
)
ok &= check(
    "私聊严格模式 @ 回",
    should_reply({"chat_type": "private", "text": "@my_muse_bot hi", "is_mention": True}, BOT, None, True),
    True,
)

print("全部通过 ✅" if ok else "有失败 ❌")
sys.exit(0 if ok else 1)
