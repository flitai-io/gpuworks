import "@ant-design/v5-patch-for-react-19";
import React from "react";
import { StyleProvider } from "@ant-design/cssinjs";
import ReactDOM from "react-dom/client";
import { ConfigProvider, theme } from "antd";
import enUS from "antd/locale/en_US";
import zhCN from "antd/locale/zh_CN";
import { BrowserRouter } from "react-router-dom";
import { useTranslation } from "react-i18next";
import App from "./App";
import "./i18n";
import { darkPreview, themeConfig } from "./theme/tokens";
import "./app.css";
import "./theme/layers.css";
// Export-only overrides, scoped to .benchmark-report-export; unlayered so they beat antd.
import "./pages/evaluations/benchmarkReportExport.css";

function Root() {
  const { i18n } = useTranslation();
  const antdLocale = i18n.resolvedLanguage === "zh" ? zhCN : enUS;

  return (
    <ConfigProvider
      locale={antdLocale}
      // Zeron buttons do not space two-character Chinese labels; antd ones match them.
      button={{ autoInsertSpace: false }}
      theme={{ algorithm: darkPreview ? theme.darkAlgorithm : theme.defaultAlgorithm, ...themeConfig }}
    >
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ConfigProvider>
  );
}

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <StyleProvider layer>
      <Root />
    </StyleProvider>
  </React.StrictMode>,
);
