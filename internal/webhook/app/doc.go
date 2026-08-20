// Package app 為 webhook-service 的應用層：定義 ports（repo / 端點來源 / HTTP 送出 / 去重 inbox / 時鐘）
// 與 use cases（IngestEvent、DispatchDue、ReapStuckInFlight、RetryDelivery、查詢），以及長駐 worker。
//
// 本套件不得 import adapter；所有 I/O 透過 ports 注入，以 fake ports 做單元測試。
package app
