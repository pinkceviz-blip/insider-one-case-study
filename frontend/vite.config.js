import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
export default defineConfig({plugins:[vue()],server:{proxy:{'/api':'http://localhost:18081','/health':'http://localhost:18081','/metrics':'http://localhost:18081'}}})
