import React from 'react';
import ReactDOM from 'react-dom/client';
import App from './App';
import { installLauncher } from './wails-launcher';
import './styles.css';

void installLauncher().then(() => {
  ReactDOM.createRoot(document.getElementById('root')!).render(
    <React.StrictMode><App /></React.StrictMode>,
  );
});
