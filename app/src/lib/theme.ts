import { createContext, useContext } from 'react';

export type Theme = 'dark' | 'light';
export const ThemeContext = createContext<Theme>('dark');
export const useTheme = () => useContext(ThemeContext);
