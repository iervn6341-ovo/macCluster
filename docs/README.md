# 文件索引

[返回專案首頁](../README.md)

## 使用與管理

| 文件 | 用途 |
| --- | --- |
| [使用與建置指南](getting-started.md) | 建置、GUI、CLI、環境與叢集操作 |
| [管理 API](management-api.md) | 本機傳輸、JSON 契約、事件與環境管理 |
| [Docker registry](docker-registry-login.md) | 映像來源與登入管理 |
| [固定埠](fixed-ports.md) | 原生工作的一個或多個固定埠 |
| [發布與隱私檢查](privacy.md) | 去識別化規則及截圖例外 |

## 網路與啟用規劃

這些文件可能包含尚未部署到現行節點的功能，請依文件日期與驗證紀錄判斷。

- [Service 網路](service-network.md)
- [出站代理](egress-design.md)
- [Service controller 正式化](service-controller-plan.md)
- [現行節點啟用規劃](production-enable-design.md)

## 進度與驗證

- [功能差距與後續階段](node-gap-roadmap.md)
- [Mac／Linux 節點比較](node-comparison.md)：包含早期比較與後續修正註記。
- [驗證紀錄](validation.md)：依日期保留測試證據與清理狀態。

## 歷史文件

早期規劃集中於 [archive/](archive)，不作為目前功能清單：

- [早期待辦清單](archive/todo.md)
- [網路方案比較](archive/network-design.md)
- [卷宗設計決策](archive/volumes-design.md)
- [Service 網路與 token 驗證規劃](archive/service-network-and-token-plan.md)

舊 `local-api.md` 的有效傳輸說明已整併至 [management-api.md](management-api.md)，不再維護兩份 API 文件。
