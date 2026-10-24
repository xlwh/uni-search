import axios from 'axios'
import { ElMessage } from 'element-plus'

const client = axios.create({
  baseURL: '/api',
  timeout: 30000,
})

client.interceptors.response.use(
  (response) => response,
  (error) => {
    const msg = error.response?.data?.error?.reason || error.message || 'Unknown error'
    ElMessage.error(msg)
    return Promise.reject(error)
  }
)

export default client
