# 测试语音

`zh-test-16k.wav` / `zh-sentence2-16k.wav`：Windows SAPI 中文语音合成（16kHz/16bit/单声道），
用于 ASR 真实链路验证。生成脚本见提交记录（PowerShell System.Speech）。

- 文件转写：`gvos-asr file -in test/audio/zh-test-16k.wav`
- 流式桥接：起 gv-core → call/start → `gvos-asr bridge -call <id>` 后台 → `gvos-asr feed -in ... -call <id>`
