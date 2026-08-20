// Package domain 為 webhook-service 的領域層：事件正規化（PaymentEvent → 對商戶的 Event）、
// delivery 狀態機與退避時程（docs/06 §4.4）、端點訂閱過濾、簽章 header 組裝與 SSRF 檢查。
//
// 本套件只依賴標準庫、pkg/ 與 api/gen（共用契約），不得 import app / adapter。
package domain
